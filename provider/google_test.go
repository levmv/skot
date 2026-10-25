package provider_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func googleResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestGoogleErrorsPreserveRetryGuidance(t *testing.T) {
	for _, test := range []struct {
		name, details, header string
		want                  time.Duration
	}{
		{name: "quota retry", header: "1", details: `[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"subject":"requests","description":"rate limit"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"52.034842833s"}]`, want: 52*time.Second + 34842833*time.Nanosecond},
		{name: "header longer", header: "60", details: `[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"10s"}]`, want: time.Minute},
		{name: "malformed details", header: "2", details: `[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"invalid"}]`, want: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"error":{"code":429,"message":"opaque provider error","details":%s}}`, test.details)
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {test.header}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			config := explicitTestConfig(provider.Gemini)
			config.HTTPClient = client
			selected, err := provider.New(config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil)
			var providerError *model.ProviderError
			if !errors.As(err, &providerError) || providerError.Kind != model.ProviderErrorRateLimit || !providerError.Retryable || providerError.RetryAfter != test.want {
				t.Fatalf("error = %v, metadata = %+v, want delay = %s", err, providerError, test.want)
			}
		})
	}
}

func TestGoogleRoutingUsesProviderCredentials(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "go-fixture")
	t.Setenv("GEMINI_API_KEY", "google-fixture")
	for _, test := range []struct {
		provider, host, prefix, key string
	}{
		{"google", "generativelanguage.googleapis.com", "/v1beta", "google-fixture"},
		{"opencode-go", "opencode.ai", "/inference/go/v1", "go-fixture"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			captured := errors.New("request captured")
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != test.host || request.URL.Path != test.prefix+"/models/test-model:streamGenerateContent" || request.URL.RawQuery != "alt=sse" {
					t.Errorf("Gemini endpoint = %s", request.URL)
				}
				if request.Header.Get("x-goog-api-key") != test.key || request.Header.Get("Authorization") != "" {
					t.Error("incorrect Gemini authorization")
				}
				if request.Header.Get("X-Session-ID") != "conversation-1" || (test.provider == "opencode-go" && request.Header.Get("x-opencode-session") != "conversation-1") {
					t.Error("missing session headers")
				}
				return nil, captured
			})}
			selected, err := provider.Open(t.Context(), provider.Config{
				URI: test.provider + "/test-model", API: provider.Gemini, HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = selected.Backend.Complete(t.Context(), model.Request{
				SessionID: "conversation-1", Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}},
			}, nil)
			if !errors.Is(err, captured) {
				t.Fatalf("request was not sent: %v", err)
			}
		})
	}
}

func TestGoogleReplaysToolHistoryAfterChanges(t *testing.T) {
	for _, change := range []string{"model", "protocol", "edited arguments"} {
		t.Run(change, func(t *testing.T) {
			config := explicitTestConfig(provider.Gemini)
			if change == "protocol" {
				config.API = provider.ChatCompletions
			}
			calls := 0
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if config.API == provider.ChatCompletions {
						return googleResponse(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"old-native-id","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"), nil
					}
					return googleResponse(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"old-native-id","name":"lookup","args":{}},"thoughtSignature":"old-signature"}]},"finishReason":"STOP"}]}` + "\n\n"), nil
				}
				var body struct {
					Contents []struct {
						Parts []struct {
							Signature string `json:"thoughtSignature"`
							Call      *struct {
								ID   string         `json:"id"`
								Name string         `json:"name"`
								Args map[string]any `json:"args"`
							} `json:"functionCall"`
							Result *struct {
								ID       string            `json:"id"`
								Name     string            `json:"name"`
								Response map[string]string `json:"response"`
							} `json:"functionResponse"`
						} `json:"parts"`
					} `json:"contents"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Contents) != 3 || len(body.Contents[1].Parts) != 1 || len(body.Contents[2].Parts) != 1 {
					t.Fatalf("lost tool history: %+v", body)
				}
				wantID := ""
				if change == "edited arguments" {
					wantID = "old-native-id"
				}
				call, result := body.Contents[1].Parts[0], body.Contents[2].Parts[0]
				if call.Signature != "skip_thought_signature_validator" || call.Call == nil || call.Call.ID != wantID || call.Call.Name != "lookup" || call.Call.Args == nil {
					t.Fatalf("invalid transferred call: %+v", call)
				}
				if change == "edited arguments" && call.Call.Args["q"] != "edited" {
					t.Fatalf("history edit was lost: %+v", call.Call.Args)
				}
				if result.Result == nil || result.Result.ID != wantID || result.Result.Name != "lookup" || result.Result.Response["output"] != "found" {
					t.Fatalf("invalid transferred result: %+v", result)
				}
				return googleResponse(`data: {"candidates":[{"content":{"parts":[{"text":"Done"}]},"finishReason":"STOP"}]}` + "\n\n"), nil
			})}
			selected, err := provider.New(config)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := (model.ReplayContext{}).ForModel(selected.Info)
			if err != nil {
				t.Fatal(err)
			}
			request := model.Request{
				Items: []model.Item{{Kind: model.ItemUserText, Text: "look it up"}},
				Tools: []model.ToolSpec{{Name: "lookup", InputSchema: jsontext.Value(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
			}
			response, err := selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err = replay.AcceptResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			request.Items = append(request.Items, response.Items...)
			for _, item := range response.Items {
				if item.ToolCall != nil {
					request.Items = append(request.Items, model.Item{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: item.ToolCall.ID, Content: model.TextContent("found")}})
					if change == "edited arguments" {
						item.ToolCall.RawArguments = `{"q":"edited"}`
					}
				}
			}
			config.API = provider.Gemini
			if change == "model" {
				config.Model = "next-model"
			}
			selected, err = provider.New(config)
			if err != nil {
				t.Fatal(err)
			}
			replay, err = replay.ForModel(selected.Info)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), nil); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
