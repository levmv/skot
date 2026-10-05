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
			selected, err := provider.Open(t.Context(), provider.Config{
				URI: test.uri, API: test.api, APIKey: "test-key", ContextWindow: 128_000, HTTPClient: client,
			})
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
				want := limit
				if want == 0 {
					want = test.defaultLimit
				}
				for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
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

func TestRequestNotSentDistinguishesCancellationFromTransportFailure(t *testing.T) {
	for _, api := range []string{"chat_completions", "responses", "anthropic_messages"} {
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
