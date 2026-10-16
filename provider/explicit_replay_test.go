package provider_test

import (
	"encoding/json/jsontext"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/provider"
)

func TestExplicitRouteReplayFollowsEndpointProtocolAndPolicy(t *testing.T) {
	var sent string
	config := explicitTestConfig(provider.ChatCompletions)
	config.ChatTraits = &chatcompletions.RouteTraits{ReasoningReplay: chatcompletions.ReasoningReplayCurrentTurn}
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		sent = string(body)
		reply := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"private-reasoning","tool_calls":[{"index":0,"id":"provider-call","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
		if request.URL.Path == "/v1/responses" {
			reply = `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}` + "\n\n"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(reply))}, nil
	})}
	first, err := provider.New(config)
	if err != nil {
		t.Fatal(err)
	}
	var replay model.ReplayContext
	replay, err = replay.ForModel(first.Info)
	if err != nil {
		t.Fatal(err)
	}
	history := []model.Item{{Kind: model.ItemUserText, Text: "look it up"}}
	tools := []model.ToolSpec{{Name: "lookup", InputSchema: jsontext.Value(`{"type":"object"}`)}}
	response, err := first.Backend.Complete(t.Context(), replay.PrepareRequest(first.Backend, model.Request{Items: history, Tools: tools}), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = replay.AcceptResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, response.Items...)
	history = append(history, model.Item{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: response.Items[1].ToolCall.ID, Content: model.TextContent("portable result")}})
	for _, test := range []struct {
		name string
		edit func(*provider.ExplicitConfig)
	}{
		{"same", func(*provider.ExplicitConfig) {}},
		{"endpoint", func(c *provider.ExplicitConfig) { c.BaseURL = "https://other.test/v1" }},
		{"protocol", func(c *provider.ExplicitConfig) { c.API, c.ChatTraits = provider.Responses, nil }},
		{"replay policy", func(c *provider.ExplicitConfig) {
			c.ChatTraits = &chatcompletions.RouteTraits{ReasoningReplay: chatcompletions.ReasoningReplayAllTurns}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			nextConfig := config
			test.edit(&nextConfig)
			next, err := provider.New(nextConfig)
			if err != nil {
				t.Fatal(err)
			}
			nextReplay, err := replay.ForModel(next.Info)
			if err != nil || (nextReplay.Epoch == replay.Epoch) != (test.name == "same") {
				t.Fatalf("replay = %+v, error = %v", nextReplay, err)
			}
			sent = ""
			if _, err := next.Backend.Complete(t.Context(), nextReplay.PrepareRequest(next.Backend, model.Request{Items: history, Tools: tools}), nil); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sent, "portable result") || !strings.Contains(sent, "lookup") {
				t.Fatalf("portable history was lost: %s", sent)
			}
			for _, private := range []string{"private-reasoning", "provider-call"} {
				if strings.Contains(sent, private) != (test.name == "same") {
					t.Fatalf("replay ownership of %s: %s", private, sent)
				}
			}
		})
	}
	if history[1].Text != "private-reasoning" || len(history[2].ToolCall.ProviderReferences) != 1 {
		t.Fatalf("projection changed saved history: %+v", history)
	}
}
