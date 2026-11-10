package provider_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (call roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return call(request)
}

func TestOutputTokenLimit(t *testing.T) {
	for _, test := range []struct {
		uri, api, field string
		defaultLimit    int
	}{
		{"openai/test", "", "max_completion_tokens", 0},
		{"deepseek/deepseek-flash", "", "max_tokens", 0},
		{"openrouter/free", "", "max_tokens", 0},
		{"ollama/test", "", "max_tokens", 0},
		{"openai/test", "responses", "max_output_tokens", 0},
		{"anthropic/claude-opus-5", "", "max_tokens", 128_000},
		{"google/test-model", "", "maxOutputTokens", 0},
		{"router/test", "chat_completions", "max_tokens", 4096},
		{"router/test", "responses", "max_output_tokens", 4096},
		{"router/test", "anthropic_messages", "max_tokens", 4096},
		{"router/test", "gemini", "maxOutputTokens", 4096},
	} {
		t.Run(test.uri+"/"+test.api, func(t *testing.T) {
			var received map[string]jsontext.Value
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if err := json.UnmarshalRead(request.Body, &received); err != nil {
					t.Fatal(err)
				}
				// Only the outgoing request matters here; no generation is performed.
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test rejection"}}`))}, nil
			})}
			config := provider.Config{
				URI: test.uri, API: test.api, APIKey: "test-key", ContextWindow: 128_000, HTTPClient: client,
			}
			if strings.HasPrefix(test.uri, "router/") {
				config.API = ""
				config.Providers = map[string]provider.ProviderConfig{"router": {
					BaseURL: new("https://router.example/v1"), API: new(test.api),
					Models: map[string]provider.ModelConfig{"test": {MaxOutputTokens: new(test.defaultLimit)}},
				}}
			}
			selected, err := provider.Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int{2048, 0, -1} {
				received = nil
				_, err := selected.Backend.Complete(t.Context(), model.Request{
					Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}, MaxOutputTokens: limit,
				}, nil)
				if limit < 0 {
					if !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrRequestNotSent) || received != nil {
						t.Fatalf("negative limit: request=%s, error=%v", received, err)
					}
					continue
				}
				if received == nil || errors.Is(err, model.ErrRequestNotSent) {
					t.Fatalf("sent request: received=%s, error=%v", received, err)
				}
				// Merge Gemini's nested generationConfig into the fields checked below.
				if config, ok := received["generationConfig"]; ok {
					if err := json.Unmarshal(config, &received); err != nil {
						t.Fatal(err)
					}
				}
				want := limit
				if want == 0 {
					want = test.defaultLimit
				}
				for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "maxOutputTokens"} {
					if field != test.field || want == 0 {
						if _, exists := received[field]; exists {
							t.Fatalf("unexpected %s: %s", field, received[field])
						}
						continue
					}
					var got int
					if err := json.Unmarshal(received[field], &got); err != nil || got != want {
						t.Fatalf("%s = %s, want %d", field, received[field], want)
					}
				}
			}
		})
	}
}

func TestNamedProviderConfigurationIsInstanceScoped(t *testing.T) {
	t.Setenv("SKOT_TEST_NAMED_KEY", "ambient-secret")
	open := func(endpoint string, window int) *provider.Model {
		t.Helper()
		sent := false
		t.Cleanup(func() {
			if !sent {
				t.Errorf("no request reached %s", endpoint)
			}
		})
		selected, err := provider.Open(t.Context(), provider.Config{
			URI: "router/Vendor/Model:channel",
			Providers: map[string]provider.ProviderConfig{"router": {
				BaseURL: new(endpoint), API: new("chat_completions"),
				APIKeyEnv: new("SKOT_TEST_NAMED_KEY"), Auth: new("none"),
				Models: map[string]provider.ModelConfig{"Vendor/Model:channel": {API: new("responses"), ContextWindow: new(window)}},
			}},
			HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				sent = true
				if request.URL.String() != endpoint+"/responses" || request.Header.Get("Authorization") != "" {
					t.Errorf("wrong instance route or unexpected authentication: %s", request.URL)
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test rejection"}}`))}, nil
			})},
		})
		if err != nil {
			t.Fatal(err)
		}
		if selected.Info.ContextWindow != window {
			t.Fatalf("context = %d, want %d", selected.Info.ContextWindow, window)
		}
		return selected
	}
	first := open("https://first.example/v1", 200_000)
	second := open("https://second.example/v1", 300_000)
	for _, selected := range []*provider.Model{first, second} {
		_, _ = selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil)
	}
	if _, err := provider.Open(t.Context(), provider.Config{URI: "router/test", APIKey: "test-key"}); err == nil {
		t.Fatal("configured provider leaked into the built-in catalog")
	}
	t.Setenv("SKOT_TEST_NAMED_KEY", "")
	_, err := provider.Open(t.Context(), provider.Config{URI: "router/test", Providers: map[string]provider.ProviderConfig{"router": {
		BaseURL: new("https://router.example/v1"), API: new("chat_completions"), APIKeyEnv: new("SKOT_TEST_NAMED_KEY"),
	}}})
	if !errors.Is(err, model.ErrInvalidRequest) || !strings.Contains(err.Error(), "SKOT_TEST_NAMED_KEY") {
		t.Fatalf("missing configured credential: %v", err)
	}
}

func TestUnverifiedRoutesPassReasoningEffortToProvider(t *testing.T) {
	for _, test := range []struct {
		name, uri, api, baseURL, effort string
		topLevel, nested, thinkingLevel string
	}{
		{name: "unknown model", uri: "openai/future-model", effort: " PROVIDER-LEVEL ", topLevel: "provider-level"},
		{name: "unreviewed protocol", uri: "deepseek/deepseek-v4-flash", api: "responses", effort: "provider-level", nested: "provider-level"},
		{name: "Gemini", uri: "google/future-model", effort: "provider-level", thinkingLevel: "provider-level"},
		{name: "Ollama", uri: "ollama/future-model", effort: "max", topLevel: "max"},
		{name: "custom OpenRouter", uri: "openrouter/vendor/future-model", baseURL: "https://gateway.example/v1", effort: "off", nested: "off"},
		{name: "custom known model", uri: "deepseek/deepseek-v4-flash", baseURL: "https://gateway.example/v1", effort: "provider-level", topLevel: "provider-level"},
		{name: "custom literal off", uri: "deepseek/deepseek-v4-flash", baseURL: "https://gateway.example/v1", effort: "off", topLevel: "off"},
		{name: "custom route without native effort", uri: "opencode-go/longcat-2.0", baseURL: "https://gateway.example/v1", effort: "max", topLevel: "max"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sent := false
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				sent = true
				var wire struct {
					Effort    string         `json:"reasoning_effort"`
					Thinking  jsontext.Value `json:"thinking"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
					GenerationConfig struct {
						ThinkingConfig struct {
							Level string `json:"thinkingLevel"`
						} `json:"thinkingConfig"`
					} `json:"generationConfig"`
				}
				if err := json.UnmarshalRead(request.Body, &wire); err != nil {
					t.Fatal(err)
				}
				if wire.Effort != test.topLevel || wire.Reasoning.Effort != test.nested ||
					wire.GenerationConfig.ThinkingConfig.Level != test.thinkingLevel || len(wire.Thinking) != 0 {
					t.Fatalf("effort changed or used the wrong encoding: %+v", wire)
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test rejection"}}`))}, nil
			})}
			selected, err := provider.Open(t.Context(), provider.Config{
				URI: test.uri, API: test.api, BaseURL: test.baseURL, ReasoningEffort: test.effort,
				APIKey: "test-key", ContextWindow: 128_000, HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selected.Backend.Complete(t.Context(), model.Request{
				Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}},
			}, nil); !sent {
				t.Fatalf("request did not reach provider: sent=%t, error=%v", sent, err)
			}
		})
	}
}

func TestReasoningEffortRejectsKnownUnsupportedValues(t *testing.T) {
	for _, config := range []provider.Config{
		{URI: "deepseek/deepseek-v4-flash", ReasoningEffort: "provider-level"},
		{URI: "opencode-go/longcat-2.0", ReasoningEffort: "high"},
		{URI: "anthropic/future-model", BaseURL: "https://gateway.example/v1", ReasoningEffort: "max"},
	} {
		if _, err := provider.Open(t.Context(), config); !errors.Is(err, model.ErrInvalidRequest) || !strings.Contains(err.Error(), "reasoning effort") {
			t.Fatalf("%s effort %q: %v", config.URI, config.ReasoningEffort, err)
		}
	}
}

func TestRequestNotSentDistinguishesCancellationFromTransportFailure(t *testing.T) {
	for _, api := range []string{"chat_completions", "responses", "anthropic_messages", "gemini"} {
		t.Run(api, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, io.ErrUnexpectedEOF
			})}
			selected, err := provider.Open(t.Context(), provider.Config{
				URI: "openai/test", API: api, APIKey: "test-key", HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = selected.Backend.Complete(ctx, request, nil)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, model.ErrRequestNotSent) || calls != 0 {
				t.Fatalf("cancelled before sending: calls=%d, error=%v", calls, err)
			}
			_, err = selected.Backend.Complete(t.Context(), request, nil)
			if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, model.ErrProviderFailure) || errors.Is(err, model.ErrRequestNotSent) || calls != 1 {
				t.Fatalf("transport failure: calls=%d, error=%v", calls, err)
			}
		})
	}
}

func TestStoredConversationSurvivesFailedModelSwitch(t *testing.T) {
	calls := 0
	var localCallID string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			Model string `json:"model"`
			Input []struct {
				ID        string `json:"id"`
				Content   string `json:"content"`
				Output    string `json:"output"`
				CallID    string `json:"call_id"`
				Encrypted string `json:"encrypted_content"`
			} `json:"input"`
		}
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Fatal(err)
		}
		var event string
		switch calls {
		case 1:
			if len(body.Input) != 1 || body.Input[0].Content != "Look it up" {
				t.Fatalf("initial input = %+v", body.Input)
			}
			event = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"reasoning","id":"rs-1","encrypted_content":"opaque-reasoning","summary":[]},{"type":"function_call","id":"fc-1","call_id":"provider-call","name":"lookup","arguments":"{}","status":"completed"}]}}`
		case 2:
			if body.Model != "second" || len(body.Input) != 3 || body.Input[1].ID != "" || body.Input[1].CallID != localCallID || body.Input[2].CallID != localCallID || body.Input[2].Output != "Found it" {
				t.Fatalf("history after switching model = %+v", body.Input)
			}
			event = `{"type":"response.failed","response":{"id":"resp-2","status":"failed","error":{"code":"server_error","message":"generation failed"}}}`
		case 3:
			if body.Model != "first" || len(body.Input) != 4 || body.Input[1].Encrypted != "opaque-reasoning" || body.Input[1].ID != "rs-1" || body.Input[2].CallID != "provider-call" || body.Input[2].ID != "fc-1" || body.Input[3].CallID != "provider-call" || body.Input[3].Output != "Found it" {
				t.Fatalf("fallback lost branch context = %+v", body.Input)
			}
			event = `{"type":"response.completed","response":{"id":"resp-3","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Here it is"}]}]}}`
		default:
			t.Fatal("backend retried a generation")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}, nil
	})}
	config := provider.Config{URI: "openai/first", API: "responses", APIKey: "explicit-key", HTTPClient: client}
	selected, err := provider.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	type history struct {
		Context model.ReplayContext
		Items   []model.Item
	}
	conversation := history{Items: []model.Item{{Kind: model.ItemUserText, Text: "Look it up"}}}
	conversation.Context, err = conversation.Context.ForModel(selected.Info)
	if err != nil {
		t.Fatal(err)
	}
	tool := model.ToolSpec{Name: "lookup", InputSchema: jsontext.Value(`{"type":"object"}`)}
	request := conversation.Context.PrepareRequest(selected.Backend, model.Request{Items: conversation.Items, Tools: []model.ToolSpec{tool}})
	response, err := selected.Backend.Complete(t.Context(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := conversation.Context.AcceptResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	localCallID = accepted.Items[1].ToolCall.ID
	if localCallID == "" || localCallID == "provider-call" {
		t.Fatal("tool call has no local identity")
	}
	conversation.Items = append(conversation.Items, accepted.Items...)
	saved, err := json.Marshal(conversation)
	if err != nil {
		t.Fatal(err)
	}
	var restored history
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Items = append(restored.Items, model.Item{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: localCallID, Content: model.TextContent("Found it")}})
	original := selected
	config.URI = "openai/second"
	selected, err = provider.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := restored.Context.ForModel(selected.Info)
	if err != nil {
		t.Fatal(err)
	}
	request = changed.PrepareRequest(selected.Backend, model.Request{Items: restored.Items, Tools: []model.ToolSpec{tool}})
	_, err = selected.Backend.Complete(t.Context(), request, nil)
	if !errors.Is(err, model.ErrProviderFailure) {
		t.Fatalf("failed attempt: %v", err)
	}
	// An unsuccessful attempt doesn't replace the branch's accepted context.
	changed, err = restored.Context.ForModel(original.Info)
	if err != nil {
		t.Fatal(err)
	}
	response, err = original.Backend.Complete(t.Context(), changed.PrepareRequest(original.Backend, model.Request{Items: restored.Items, Tools: []model.ToolSpec{tool}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Items[0].Text != "Here it is" {
		t.Fatalf("fallback response = %+v", response)
	}
}
