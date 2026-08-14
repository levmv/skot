package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

type appRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn appRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestOpenCodeGoRoutesUseDeclaredProtocolTraitsEndpointAndCredential(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "subscription-secret")
	tests := []struct {
		name                string
		uri                 string
		effort              string
		path                string
		protocol            modelAPI
		request             agent.ModelRequest
		wantReplayReasoning string
		checkReplay         bool
		writeBody           func(io.Writer)
		checkBody           func(*testing.T, map[string]json.RawMessage)
	}{
		{
			name: "chat completions tool-turn replay", uri: "opencode-go/deepseek-v4-flash", effort: "high",
			path: "/zen/go/v1/chat/completions", protocol: modelAPIChatCompletions,
			request:             openCodeGoReplayRequest(),
			wantReplayReasoning: "tool reasoning",
			checkReplay:         true,
			writeBody: func(writer io.Writer) {
				fmt.Fprint(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			},
			checkBody: func(t *testing.T, body map[string]json.RawMessage) {
				t.Helper()
				if string(body["reasoning_effort"]) != `"high"` {
					t.Errorf("reasoning_effort = %s", body["reasoning_effort"])
				}
				if _, exists := body["prompt_cache_key"]; exists {
					t.Error("OpenCode Go chat request contains OpenAI-only prompt_cache_key")
				}
			},
		},
		{
			name: "chat completions current-turn replay", uri: "opencode-go/kimi-k3", effort: "max",
			path: "/zen/go/v1/chat/completions", protocol: modelAPIChatCompletions,
			request:     openCodeGoReplayRequest(),
			checkReplay: true,
			writeBody: func(writer io.Writer) {
				fmt.Fprint(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			},
			checkBody: func(t *testing.T, body map[string]json.RawMessage) {
				t.Helper()
				if string(body["reasoning_effort"]) != `"max"` {
					t.Errorf("reasoning_effort = %s", body["reasoning_effort"])
				}
			},
		},
		{
			name: "responses reasoning controls", uri: "opencode-go/gpt-5.6-luna", effort: "high",
			path: "/zen/go/v1/responses", protocol: modelAPIResponses,
			request: agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "reply ok"}}},
			writeBody: func(writer io.Writer) {
				fmt.Fprint(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
				fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
			},
			checkBody: func(t *testing.T, body map[string]json.RawMessage) {
				t.Helper()
				if string(body["store"]) != "false" || string(body["stream"]) != "true" {
					t.Errorf("Responses flags = store:%s stream:%s", body["store"], body["stream"])
				}
				if _, exists := body["include"]; exists {
					t.Error("Responses request contains legacy encrypted-content include")
				}
				var reasoning map[string]json.RawMessage
				if err := json.Unmarshal(body["reasoning"], &reasoning); err != nil {
					t.Errorf("decode reasoning config: %v", err)
				}
				if string(reasoning["effort"]) != `"high"` || string(reasoning["summary"]) != `"auto"` {
					t.Errorf("reasoning config = %s", body["reasoning"])
				}
				if _, exists := reasoning["context"]; exists {
					t.Error("request sends an undeclared reasoning.context")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Errorf("server path = %q", request.URL.Path)
				}
				if authorization := request.Header.Get("Authorization"); authorization != "Bearer subscription-secret" {
					t.Errorf("authorization = %q", authorization)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if string(body["model"]) != `"`+strings.TrimPrefix(test.uri, "opencode-go/")+`"` {
					t.Errorf("model = %s", body["model"])
				}
				test.checkBody(t, body)
				if test.checkReplay {
					var messages []struct {
						Role             string `json:"role"`
						ReasoningContent string `json:"reasoning_content"`
					}
					if err := json.Unmarshal(body["messages"], &messages); err != nil {
						t.Errorf("decode messages: %v", err)
					}
					var replayed string
					for _, message := range messages {
						if message.Role == "assistant" {
							replayed += message.ReasoningContent
						}
					}
					if replayed != test.wantReplayReasoning {
						t.Errorf("replayed reasoning = %q, want %q", replayed, test.wantReplayReasoning)
					}
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				test.writeBody(writer)
			}))
			defer server.Close()

			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			transport := server.Client().Transport
			client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Scheme != "https" || request.URL.Host != "opencode.ai" || request.URL.Path != test.path {
					t.Errorf("provider request URL = %s", request.URL)
				}
				clone := request.Clone(request.Context())
				requestURL := *request.URL
				requestURL.Scheme = target.Scheme
				requestURL.Host = target.Host
				clone.URL = &requestURL
				clone.Host = target.Host
				return transport.RoundTrip(clone)
			})}

			route, err := resolveModelRoute(test.uri, test.effort, modelRouteOverrides{}, modelRouteEnrichment{})
			if err != nil {
				t.Fatal(err)
			}
			if route.API != test.protocol || route.Compatibility != modelCompatibilitySupported ||
				route.BaseURL != "https://opencode.ai/zen/go/v1" {
				t.Fatalf("route = %#v", route)
			}
			backend, err := buildModelBackend(route, nil, modelBackendOptions{requireCredential: true, httpClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if endpoint := backend.Info().Endpoint; endpoint != "https://opencode.ai/zen/go/v1" {
				t.Fatalf("backend endpoint = %q", endpoint)
			}
			response, err := backend.Complete(context.Background(), test.request, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Items) != 1 || response.Items[0].Kind != agent.ItemAssistantText || response.Items[0].Text != "ok" {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func openCodeGoReplayRequest() agent.ModelRequest {
	return agent.ModelRequest{
		ProviderEpoch: "epoch_1",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "first"},
			{
				Kind: agent.ItemReasoning, ResponseID: "response_1", Text: "tool reasoning",
				ProviderContext: &agent.ProviderContext{Backend: "chat_completions.opencode-go", Epoch: "epoch_1"},
			},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{ID: "call_1", Name: "read", RawArguments: `{"path":"README.md"}`}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_1", Content: "contents"}},
			{Kind: agent.ItemUserText, Text: "second"},
		},
	}
}
