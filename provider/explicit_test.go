package provider_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/model/responses"
	"github.com/levmv/skot/model/transport"
	"github.com/levmv/skot/provider"
)

func TestExplicitModelUsesOnlyCallerConfiguration(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	for _, test := range []struct {
		api  provider.API
		auth string
	}{
		{provider.ChatCompletions, "key"},
		{provider.Responses, "key"},
		{provider.AnthropicMessages, "key"},
		{provider.Gemini, "key"},
		{provider.Responses, "anonymous"},
		{provider.Responses, "authorizer"},
	} {
		api, auth := test.api, test.auth
		t.Run(string(api)+"/"+auth, func(t *testing.T) {
			calls, authorized := 0, 0
			config := explicitTestConfig(api)
			// The name deliberately disagrees with some protocols. It must not
			// select OpenAI credentials or OpenAI's Chat Completions dialect.
			config.Provider, config.Model = "openai", "Unlisted/Exact-ID"
			config.Header = http.Header{"X-Route": {"original"}}
			if api != provider.AnthropicMessages {
				config.ReasoningEffort = "medium"
			}
			switch auth {
			case "key":
				config.APIKey = "caller-key"
			case "authorizer":
				config.Authorizer = transport.AuthorizerFunc(func(_ context.Context, request *http.Request) error {
					authorized++
					request.Header.Set("X-Custom-Key", "caller-key")
					return nil
				})
			}
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Header.Get("X-Route") != "original" {
					t.Fatalf("route headers were not forwarded: %v", request.Header)
				}
				keyHeader := "Authorization"
				keyValue := "Bearer caller-key"
				if api == provider.AnthropicMessages {
					keyHeader, keyValue = "x-api-key", "caller-key"
				} else if api == provider.Gemini {
					keyHeader, keyValue = "x-goog-api-key", "caller-key"
				}
				for _, header := range []string{"Authorization", "x-api-key", "x-goog-api-key", "X-Custom-Key"} {
					want := ""
					if auth == "key" && header == keyHeader {
						want = keyValue
					} else if auth == "authorizer" && header == "X-Custom-Key" {
						want = "caller-key"
					}
					if got := request.Header.Get(header); got != want {
						t.Fatalf("%s = %q, want %q", header, got, want)
					}
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if calls == 1 {
					var wire struct {
						Model           string `json:"model"`
						ReasoningEffort string `json:"reasoning_effort"`
						MaxTokens       int    `json:"max_tokens"`
						Reasoning       struct {
							Effort string `json:"effort"`
						} `json:"reasoning"`
						GenerationConfig struct {
							ThinkingConfig struct {
								Level string `json:"thinkingLevel"`
							} `json:"thinkingConfig"`
						} `json:"generationConfig"`
					}
					if err := json.Unmarshal(body, &wire); err != nil {
						t.Fatal(err)
					}
					path, effort := "", ""
					switch api {
					case provider.ChatCompletions:
						path, effort = "/v1/chat/completions", wire.ReasoningEffort
					case provider.Responses:
						path, effort = "/v1/responses", wire.Reasoning.Effort
					case provider.AnthropicMessages:
						path = "/v1/messages"
						if wire.MaxTokens != config.Anthropic.MaxOutputTokens {
							t.Fatalf("Messages output limit = %d", wire.MaxTokens)
						}
					case provider.Gemini:
						path, effort = "/v1/models/Unlisted%2FExact-ID:streamGenerateContent?alt=sse", wire.GenerationConfig.ThinkingConfig.Level
					}
					if request.URL.String() != "https://example.test"+path {
						t.Fatalf("generation URL = %s", request.URL)
					}
					if api != provider.Gemini && wire.Model != config.Model {
						t.Fatalf("wire model = %q", wire.Model)
					}
					if effort != config.ReasoningEffort {
						t.Fatalf("effort = %q, want %q: %s", effort, config.ReasoningEffort, body)
					}
				} else if string(body) != `{"model":"raw-caller-model"}` {
					t.Fatalf("raw request was changed: %s", body)
				}
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"credential rejected"}}`))}, nil
			})}
			selected, err := provider.New(config)
			if err != nil || calls != 0 || authorized != 0 {
				t.Fatalf("construction: model=%v, calls=%d, authorizations=%d, error=%v", selected, calls, authorized, err)
			}
			if selected.Info.Model != config.Model || selected.Info.Provider != config.Provider || selected.Info.ContextWindow != config.ContextWindow ||
				selected.Info.ContextWindowEstimated || selected.Info.Endpoint != config.BaseURL || selected.Info.ReasoningEffort != config.ReasoningEffort ||
				selected.Connection.APIModel() != config.Model || selected.Connection.API() != string(api) {
				t.Fatalf("prepared route = %+v, %s/%s", selected.Info, selected.Connection.API(), selected.Connection.APIModel())
			}
			_, completeErr := selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil)
			raw, postErr := selected.Connection.Post(t.Context(), []byte(`{"model":"raw-caller-model"}`), nil)
			if raw != nil {
				defer raw.Body.Close()
			}
			for _, err := range []error{completeErr, postErr} {
				var providerErr *model.ProviderError
				if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusUnauthorized || providerErr.Kind != model.ProviderErrorAuthentication || errors.Is(err, model.ErrRequestNotSent) {
					t.Fatalf("request error = %v", err)
				}
			}
			if calls != 2 || (auth == "authorizer" && authorized != 2) {
				t.Fatalf("requests=%d, authorizations=%d", calls, authorized)
			}
		})
	}
}

func TestExplicitModelRejectsUnusableConfigurationLocally(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*provider.ExplicitConfig)
	}{
		{"missing provider", func(c *provider.ExplicitConfig) { c.Provider = "" }},
		{"missing model", func(c *provider.ExplicitConfig) { c.Model = "" }},
		{"missing API", func(c *provider.ExplicitConfig) { c.API = "" }},
		{"unknown API", func(c *provider.ExplicitConfig) { c.API = "unknown" }},
		{"missing context", func(c *provider.ExplicitConfig) { c.ContextWindow = 0 }},
		{"relative URL", func(c *provider.ExplicitConfig) { c.BaseURL = "/v1" }},
		{"URL credentials", func(c *provider.ExplicitConfig) { c.BaseURL = "https://key@example.test/v1" }},
		{"URL routing query", func(c *provider.ExplicitConfig) { c.BaseURL += "?route=other" }},
		{"URL fragment", func(c *provider.ExplicitConfig) { c.BaseURL += "#" }},
		{"wrong protocol options", func(c *provider.ExplicitConfig) { c.ResponsesTraits = &responses.RouteTraits{} }},
		{"unsupported effort encoding", func(c *provider.ExplicitConfig) {
			c.ChatTraits = &chatcompletions.RouteTraits{}
			c.ReasoningEffort = "medium"
		}},
		{"missing Messages limit", func(c *provider.ExplicitConfig) { c.API = provider.AnthropicMessages }},
		{"Messages effort", func(c *provider.ExplicitConfig) {
			c.API, c.ReasoningEffort = provider.AnthropicMessages, "medium"
			c.Anthropic = &provider.AnthropicOptions{MaxOutputTokens: 1024}
		}},
		{"unknown response profile", func(c *provider.ExplicitConfig) { c.ResponseProfile = "unknown" }},
		{"conflicting credentials", func(c *provider.ExplicitConfig) {
			c.APIKey, c.Authorizer = "key", transport.BearerToken("other")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := explicitTestConfig(provider.ChatCompletions)
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("constructor sent an HTTP request")
				return nil, nil
			})}
			test.edit(&config)
			if _, err := provider.New(config); !errors.Is(err, model.ErrInvalidRequest) {
				t.Fatalf("configuration error = %v", err)
			}
		})
	}
}

func TestResponseProfileAppliesToBackendAndRawObserverIndependentlyOfName(t *testing.T) {
	const first = `{"id":"gen-1","choices":[{"index":0,"delta":{"content":"partial"}}]}`
	const last = `{"error":{"code":402,"message":"credits exhausted"},"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.02}}`
	for _, name := range []string{"custom", "openrouter"} {
		for _, profile := range []model.ResponseProfile{"", model.ResponseProfileOpenRouter} {
			t.Run(name+"/"+string(profile), func(t *testing.T) {
				config := explicitTestConfig(provider.ChatCompletions)
				config.Provider, config.Model, config.ResponseProfile = name, "free", profile
				config.ReasoningEffort = "medium"
				calls := 0
				config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						var wire struct {
							ReasoningEffort string         `json:"reasoning_effort"`
							Reasoning       jsontext.Value `json:"reasoning"`
						}
						if err := json.UnmarshalRead(request.Body, &wire); err != nil || wire.ReasoningEffort != "medium" || len(wire.Reasoning) != 0 {
							t.Fatalf("connection name or response profile changed the request dialect: %+v, %v", wire, err)
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"req-1"}}, Body: io.NopCloser(strings.NewReader("data: " + first + "\n\ndata: " + last + "\n\n"))}, nil
				})}
				selected, err := provider.New(config)
				if err != nil || calls != 0 {
					t.Fatalf("construct: calls=%d, err=%v", calls, err)
				}
				if selected.Connection.APIModel() != "free" || selected.Info.Model != "free" {
					t.Fatal("explicit model was resolved as a catalog alias")
				}
				var streamed string
				response, completeErr := selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, func(event model.StreamEvent) { streamed += event.Text })
				if streamed != "partial" || len(response.Items) != 0 {
					t.Fatalf("failed output: streamed=%q, items=%v", streamed, response.Items)
				}
				raw, err := selected.Connection.Post(t.Context(), []byte(`{"model":"free","stream":true}`), nil)
				if err != nil {
					t.Fatal(err)
				}
				stream := transport.OpenEventStream(t.Context(), raw.Body, transport.StreamOptions{})
				defer stream.Close()
				observer := chatcompletions.NewObserver(selected.Connection)
				observer.ObserveHeader(raw.Header)
				var observeErr error
				for range 2 {
					event, err := stream.Next()
					if err != nil {
						t.Fatal(err)
					}
					observeErr = observer.ObserveChunk(event.Data)
					if observeErr != nil {
						break
					}
				}
				for _, err := range []error{completeErr, observeErr} {
					var providerErr *model.ProviderError
					if !errors.As(err, &providerErr) || providerErr.StatusCode != 0 || providerErr.Code != "402" {
						t.Fatalf("in-band error = %v", err)
					}
					want := model.ProviderErrorKind("")
					if profile == model.ResponseProfileOpenRouter {
						want = model.ProviderErrorQuota
					}
					if providerErr.Kind != want || providerErr.Retryable != (want == "") {
						t.Fatalf("classification = %+v", providerErr)
					}
				}
				usage := response.Usage
				if !reflect.DeepEqual(usage, observer.Usage()) || usage.Tokens.Known().TotalTokens != 12 || usage.RequestID != "req-1" {
					t.Fatalf("backend/raw usage = %+v / %+v", usage, observer.Usage())
				}
				if profile == model.ResponseProfileOpenRouter {
					if !reflect.DeepEqual(usage.Costs, []model.ReportedCost{{Kind: "account_charge", Amount: "0.02", Currency: "USD"}}) {
						t.Fatalf("costs = %+v", usage.Costs)
					}
				} else if len(usage.Costs) != 0 {
					t.Fatalf("unselected monetary extension = %+v", usage.Costs)
				}
				if calls != 2 {
					t.Fatalf("requests = %d", calls)
				}
			})
		}
	}
}

func explicitTestConfig(api provider.API) provider.ExplicitConfig {
	config := provider.ExplicitConfig{Provider: "custom", Model: "model", API: api, BaseURL: "https://example.test/v1", ContextWindow: 128_000}
	if api == provider.AnthropicMessages {
		config.Anthropic = &provider.AnthropicOptions{MaxOutputTokens: 2048}
	}
	return config
}
