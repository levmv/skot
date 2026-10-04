package anthropic

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levmv/skot/model"
)

func TestCompleteStreamsContentToolsAndUsage(t *testing.T) {
	var received struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema jsontext.Value `json:"input_schema"`
		} `json:"tools"`
		Stream bool `json:"stream"`
	}
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if key := request.Header.Get("x-api-key"); key != "secret" {
			t.Errorf("x-api-key = %q", key)
		}
		if version := request.Header.Get("anthropic-version"); version != anthropicVersion {
			t.Errorf("anthropic-version = %q", version)
		}
		if value := request.Header.Get("X-Test"); value != "yes" {
			t.Errorf("X-Test = %q", value)
		}
		if err := json.UnmarshalRead(request.Body, &received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "message_start",
			"message": map[string]any{"usage": map[string]any{
				"input_tokens": 12, "output_tokens": 1,
				"cache_read_input_tokens": 4, "cache_creation_input_tokens": 3,
			}},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "ping"})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": "checking "},
		})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "signature_delta", "signature": "signed-thinking"},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "content_block_stop", "index": 0})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_delta", "index": 1,
			"delta": map[string]any{"type": "text_delta", "text": "hello"},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "content_block_stop", "index": 1})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_start", "index": 2,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_provider_1", "name": "read_file", "input": map[string]any{}},
		})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_delta", "index": 2,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":`},
		})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_delta", "index": 2,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `"README.md"}`},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "content_block_stop", "index": 2})
		writeSSEEvent(t, writer, map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"},
			"usage": map[string]any{"output_tokens": 9},
		})
		// Unknown events are allowed by the protocol versioning policy.
		writeSSEEvent(t, writer, map[string]any{"type": "future_event", "value": true})
		writeSSEEvent(t, writer, map[string]any{"type": "message_stop"})
	}))

	backend := newTestServerBackend(t, server, "/v1")
	var events []model.StreamEvent
	response, err := backend.Complete(context.Background(), model.Request{
		Instructions: "be brief", Summary: "prior turn",
		Items: []model.Item{{Kind: model.ItemUserText, Text: "read it"}},
		Tools: []model.ToolSpec{{Name: "read_file", Description: "Read a file", InputSchema: jsontext.Value(`{"type":"object"}`)}},
	}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	t.Run("request", func(t *testing.T) {
		if received.Model != "wire-model" || received.MaxTokens != 131_072 || !received.Stream || received.System != "be brief\n\nConversation summary:\nprior turn" {
			t.Fatalf("request = %#v", received)
		}
		if len(received.Messages) != 1 || received.Messages[0].Role != "user" || len(received.Messages[0].Content) != 1 ||
			received.Messages[0].Content[0].Type != "text" || received.Messages[0].Content[0].Text != "read it" {
			t.Fatalf("messages = %#v", received.Messages)
		}
		if len(received.Tools) != 1 || received.Tools[0].Name != "read_file" || string(received.Tools[0].InputSchema) != `{"type":"object"}` {
			t.Fatalf("tools = %#v", received.Tools)
		}
	})
	t.Run("response", func(t *testing.T) {
		// Messages says tool_use; the agent sees the normalized reason.
		if response.StopReason != "tool_calls" || response.Usage.Tokens.Known() != (model.TokenCounts{
			InputTokens: 19, CachedInputTokens: 4, CacheWriteInputTokens: 3, OutputTokens: 9, TotalTokens: 28,
		}) {
			t.Fatalf("stop/usage = %q/%#v", response.StopReason, response.Usage)
		}
		if len(response.Items) != 3 || response.Items[0].Kind != model.ItemReasoning || response.Items[0].Text != "checking " ||
			response.Items[1].Kind != model.ItemAssistantText || response.Items[1].Text != "hello" || response.Items[2].ToolCall == nil {
			t.Fatalf("items = %#v", response.Items)
		}
		if len(response.Items[0].ProviderData) != 1 || response.Items[0].ProviderData[0].Kind != thinkingDataKind {
			t.Fatalf("thinking provider data = %#v", response.Items[0].ProviderData)
		}
		var thinkingState thinkingBlockState
		if err := json.Unmarshal(response.Items[0].ProviderData[0].Data, &thinkingState); err != nil ||
			thinkingState.Type != "thinking" || thinkingState.Thinking != "checking " || thinkingState.Signature != "signed-thinking" {
			t.Fatalf("thinking state = %#v, error = %v", thinkingState, err)
		}
		call := response.Items[2].ToolCall
		if call.Name != "read_file" || call.RawArguments != `{"path":"README.md"}` || len(call.ProviderReferences) != 1 ||
			call.ProviderReferences[0].Kind != "anthropic_messages.test.tool_use_id" {
			t.Fatalf("tool call = %#v", call)
		}
		var providerID string
		if err := json.Unmarshal(call.ProviderReferences[0].Data, &providerID); err != nil || providerID != "toolu_provider_1" {
			t.Fatalf("provider ID = %q, error = %v", providerID, err)
		}
		if got := events; !reflect.DeepEqual(got, []model.StreamEvent{
			{Kind: model.EventReasoningSummaryDelta, Text: "checking "},
			{Kind: model.EventTextDelta, Text: "hello"},
		}) {
			t.Fatalf("events = %#v", got)
		}
	})
}

func TestBuildRequestMapsHistoryAndProviderToolID(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	providerID := jsontext.Value(`"toolu_provider_7"`)
	thinkingState := jsontext.Value(`{"type":"thinking","thinking":"original thinking","signature":"signed-thinking"}`)
	request, err := backend.buildRequest(model.Request{
		Instructions: "instructions", Summary: "summary", ProviderEpoch: "epoch_1",
		Items: []model.Item{
			{Kind: model.ItemUserText, Text: "first"},
			{
				Kind: model.ItemReasoning, ResponseID: "response_1", Text: "sanitized thinking",
				ProviderContext: &model.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"},
				ProviderData:    []model.ProviderData{{Kind: thinkingDataKind, Data: thinkingState}},
			},
			{Kind: model.ItemAssistantText, ResponseID: "response_1", Text: "checking"},
			{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{
				ID: "call_local", Name: "read_file", RawArguments: `{"path":"README.md"}`,
				ProviderReferences: []model.ProviderReference{{
					Kind: "anthropic_messages.test.tool_use_id", Backend: "anthropic_messages.test", Epoch: "epoch_1", Data: providerID,
				}},
			}},
			{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call_local", Content: model.TextContent("failed"), Error: true}},
			{Kind: model.ItemBoundaryText, Text: "Background job completed."},
			{Kind: model.ItemUserText, Text: "continue"},
		},
		Tools: []model.ToolSpec{{Name: "read_file", InputSchema: jsontext.Value(`{"type":"object","additionalProperties":false}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.System != "instructions\n\nConversation summary:\nsummary" || len(request.Messages) != 3 {
		t.Fatalf("request = %#v", request)
	}
	assistant := request.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 3 || assistant.Content[0].Type != "thinking" ||
		assistant.Content[0].Thinking == nil || *assistant.Content[0].Thinking != "original thinking" || assistant.Content[0].Signature != "signed-thinking" ||
		assistant.Content[1].Text != "checking" || assistant.Content[2].Type != "tool_use" ||
		assistant.Content[2].ID != "toolu_provider_7" || string(assistant.Content[2].Input) != `{"path":"README.md"}` {
		t.Fatalf("assistant message = %#v", assistant)
	}
	user := request.Messages[2]
	if user.Role != "user" || len(user.Content) != 3 || user.Content[0].Type != "tool_result" ||
		user.Content[0].ToolUseID != "toolu_provider_7" || user.Content[0].Content != "failed" || !user.Content[0].IsError ||
		user.Content[1].Text != "Background job completed." || user.Content[2].Text != "continue" {
		t.Fatalf("user message = %#v", user)
	}
}

func TestBuildRequestLowersImageToolResult(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	request, err := backend.buildRequest(model.Request{Items: []model.Item{
		{Kind: model.ItemUserText, Text: "inspect"},
		{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{ID: "call_1", Name: "read", RawArguments: `{"path":"shot.png"}`}},
		{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call_1", Content: model.ImageToolContent("image metadata", model.ImageContent{
			MediaType: "image/png", Data: []byte{1, 2, 3}, Width: 10, Height: 5,
		})}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := request.Messages[len(request.Messages)-1].Content[0]
	parts, ok := result.Content.([]toolResultContentBlock)
	if result.Type != "tool_result" || !ok || len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "image metadata" ||
		parts[1].Type != "image" || parts[1].Source == nil || parts[1].Source.MediaType != "image/png" || string(parts[1].Source.Data) != string([]byte{1, 2, 3}) {
		t.Fatalf("tool result = %#v", result)
	}
}

func TestBuildRequestPreservesEmptyThinkingAndToolResultFields(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	state := jsontext.Value(`{"type":"thinking","signature":"sig-abc"}`)
	request, err := backend.buildRequest(model.Request{
		ProviderEpoch: "epoch_1",
		Items: []model.Item{
			{Kind: model.ItemUserText, Text: "run the tool"},
			{
				Kind: model.ItemReasoning, ResponseID: "response_1",
				ProviderContext: &model.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"},
				ProviderData:    []model.ProviderData{{Kind: thinkingDataKind, Data: state}},
			},
			{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{
				ID: "call_1", Name: "empty", RawArguments: `{}`,
			}},
			{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call_1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Content []map[string]jsontext.Value `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 3 || len(wire.Messages[1].Content) != 2 ||
		string(wire.Messages[1].Content[0]["thinking"]) != `""` ||
		string(wire.Messages[1].Content[0]["signature"]) != `"sig-abc"` ||
		len(wire.Messages[2].Content) != 1 || string(wire.Messages[2].Content[0]["content"]) != `""` {
		t.Fatalf("wire messages = %s", payload)
	}
}

func TestBuildRequestDropsMismatchedThinkingState(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	state := jsontext.Value(`{"type":"thinking","signature":"signature"}`)
	request, err := backend.buildRequest(model.Request{
		ProviderEpoch: "epoch_1",
		Items: []model.Item{
			{
				Kind: model.ItemReasoning, ResponseID: "response_1", Text: "private",
				ProviderContext: &model.ProviderContext{Backend: "anthropic_messages.other", Epoch: "epoch_1"},
				ProviderData:    []model.ProviderData{{Kind: thinkingDataKind, Data: state}},
			},
			{Kind: model.ItemAssistantText, ResponseID: "response_1", Text: "visible"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 || request.Messages[0].Content[0].Text != "visible" {
		t.Fatalf("messages = %#v", request.Messages)
	}
}

func TestRedactedThinkingRoundTripsAsProviderState(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	items, err := backend.responseItems(map[int]*streamBlock{
		0: {kind: "redacted_thinking", data: jsontext.Value(`"opaque-state"`), closed: true},
	}, "stop")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != model.ItemReasoning || items[0].Text != "" || len(items[0].ProviderData) != 1 {
		t.Fatalf("items = %#v", items)
	}
	items[0].ResponseID = "response_1"
	items[0].ProviderContext = &model.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"}
	request, err := backend.buildRequest(model.Request{ProviderEpoch: "epoch_1", Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 ||
		request.Messages[0].Content[0].Type != "redacted_thinking" || string(request.Messages[0].Content[0].Data) != `"opaque-state"` {
		t.Fatalf("messages = %#v", request.Messages)
	}
}

func TestInvalidRedactedThinkingDataIsDroppedAndRejectedWhenSaved(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	for _, data := range []jsontext.Value{jsontext.Value(`null`), jsontext.Value(`{}`), jsontext.Value(`123`), jsontext.Value(`""`)} {
		t.Run(string(data), func(t *testing.T) {
			items, err := backend.responseItems(map[int]*streamBlock{
				0: {kind: "redacted_thinking", data: data, closed: true},
			}, "stop")
			if err != nil || len(items) != 0 {
				t.Fatalf("items/error = %#v/%v", items, err)
			}

			state, err := json.Marshal(thinkingBlockState{Type: "redacted_thinking", Data: data})
			if err != nil {
				t.Fatal(err)
			}
			_, replayed, err := backend.replayThinkingBlock(model.Item{
				ProviderContext: &model.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"},
				ProviderData:    []model.ProviderData{{Kind: thinkingDataKind, Data: state}},
			}, "epoch_1")
			if err == nil || replayed || !strings.Contains(err.Error(), "no opaque data") {
				t.Fatalf("replayed/error = %t/%v", replayed, err)
			}
		})
	}
}

func TestResponseItemsSortSparseProviderIndices(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	blocks := map[int]*streamBlock{
		5_000: {kind: "text", closed: true},
		2:     {kind: "text", closed: true},
	}
	blocks[5_000].text.WriteString("late")
	blocks[2].text.WriteString("early")
	items, err := backend.responseItems(blocks, "stop")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(items, []model.Item{
		{Kind: model.ItemAssistantText, Text: "early"},
		{Kind: model.ItemAssistantText, Text: "late"},
	}) {
		t.Fatalf("items = %#v", items)
	}
}

func TestCompletePreservesPartialTextAtLocalOutputLimit(t *testing.T) {
	start := mustJSON(t, map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	kept := mustJSON(t, map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "kept"},
	})
	dropped := mustJSON(t, map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "dropped"},
	})
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: %s\n\ndata: %s\n\ndata: %s\n\n", start, kept, dropped)
	}))
	backend := newTestServerBackend(t, server, "")
	backend.maxCompletionBytes = len(start) + len(kept)
	response, err := backend.Complete(context.Background(), model.Request{
		Items: []model.Item{{Kind: model.ItemUserText, Text: "continue"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != model.StopReasonOutputLimit || !reflect.DeepEqual(response.Items, []model.Item{{Kind: model.ItemAssistantText, Text: "kept"}}) {
		t.Fatalf("response = %#v", response)
	}
}

func TestCompleteRejectsOversizedRequestWithoutSendingIt(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	backend := newTestServerBackend(t, server, "")
	backend.maxRequestBytes = 64
	_, err := backend.Complete(context.Background(), model.Request{
		Items: []model.Item{{Kind: model.ItemUserText, Text: strings.Repeat("x", 128)}},
	}, nil)
	if !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrModelRequestTooLarge) || requests.Load() != 0 {
		t.Fatalf("error/requests = %v/%d", err, requests.Load())
	}
}

func TestCompletePreservesPartialOutputWithoutToolCalls(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		wantError    bool
	}{
		{name: "token limit", reason: "max_tokens"},
		{name: "malformed completed call", reason: "tool_use", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []map[string]any{
					{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 10, "output_tokens": 0}}},
					{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "checking", "signature": "signed-thinking"}},
					{"type": "content_block_stop", "index": 0},
					{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": "partial"}},
					{"type": "content_block_stop", "index": 1},
					{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read", "input": map[string]any{"path": "first"}}},
					{"type": "content_block_stop", "index": 2},
					{"type": "content_block_start", "index": 3, "content_block": map[string]any{"type": "tool_use", "id": "toolu_2", "name": "read", "input": map[string]any{}}},
					{"type": "content_block_delta", "index": 3, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"unfinished`}},
					{"type": "content_block_stop", "index": 3},
					{"type": "message_delta", "delta": map[string]any{"stop_reason": test.reason}, "usage": map[string]any{"output_tokens": 3}},
					{"type": "message_stop"},
				} {
					writeSSEEvent(t, writer, event)
				}
			}))
			backend := newTestServerBackend(t, server, "")
			response, err := backend.Complete(context.Background(), model.Request{}, nil)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
					t.Fatalf("malformed completed call error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Items) != 2 || response.Items[0].Kind != model.ItemReasoning || response.Items[0].Text != "checking" ||
				len(response.Items[0].ProviderData) != 1 || response.Items[1].Kind != model.ItemAssistantText || response.Items[1].Text != "partial" ||
				response.StopReason != test.reason || response.Usage.Tokens.Known() != (model.TokenCounts{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}) {
				t.Fatalf("incomplete response = %#v", response)
			}
			var thinking thinkingBlockState
			if err := json.Unmarshal(response.Items[0].ProviderData[0].Data, &thinking); err != nil ||
				thinking.Type != "thinking" || thinking.Thinking != "checking" || thinking.Signature != "signed-thinking" {
				t.Fatalf("saved thinking = %#v, error = %v", thinking, err)
			}
		})
	}
}

func TestCompleteDecodesStructuredHTTPError(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "2")
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(writer, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != model.ProviderErrorAuthentication ||
		providerErr.Code != "" || providerErr.Type != "authentication_error" ||
		providerErr.StatusCode != http.StatusUnauthorized || providerErr.RetryAfter != 2*time.Second {
		t.Fatalf("error = %#v (%v)", providerErr, err)
	}
}

func TestCompleteReturnsStreamError(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "overloaded"},
		})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	if !errors.Is(err, model.ErrProviderFailure) || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteClassifiesStructuredStreamContextLimit(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "error", "error": map[string]any{"type": "request_too_large", "message": "opaque detail"},
		})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	var providerErr *model.ProviderError
	if !errors.Is(err, model.ErrModelRequestTooLarge) || !errors.As(err, &providerErr) ||
		providerErr.Kind != model.ProviderErrorRequestTooLarge || providerErr.Type != "request_too_large" ||
		providerErr.Retryable {
		t.Fatalf("error/metadata = %v / %#v", err, providerErr)
	}
}

func TestCompleteReturnsEmptyRefusalForRuntimeClassification(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal"},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "message_stop"})
	}))
	backend := newTestServerBackend(t, server, "")
	response, err := backend.Complete(context.Background(), model.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "refusal" || len(response.Items) != 0 {
		t.Fatalf("response = %#v", response)
	}
}

func TestCompleteHonorsStreamIdleTimeout(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-request.Context().Done()
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{StreamIdleTimeout: 10 * time.Millisecond}, nil)
	if !errors.Is(err, model.ErrModelStreamIdle) {
		t.Fatalf("error = %v", err)
	}
	<-started
}

func TestCompleteHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-request.Context().Done()
	}))
	backend := newTestServerBackend(t, server, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backend.Complete(ctx, model.Request{}, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteRejectsStreamWithoutTerminalEvent(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		writeSSEEvent(t, writer, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "partial"},
		})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	if !errors.Is(err, model.ErrProviderFailure) || !strings.Contains(err.Error(), "before message_stop") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteRejectsMalformedStreamState(t *testing.T) {
	tests := []struct {
		name   string
		events []map[string]any
		want   string
	}{
		{
			name: "negative block index",
			events: []map[string]any{{
				"type": "content_block_start", "index": -1,
				"content_block": map[string]any{"type": "text"},
			}},
			want: "content block index -1 is negative",
		},
		{
			name: "repeated block index",
			events: []map[string]any{
				{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}},
				{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}},
			},
			want: "repeated content block index 0",
		},
		{
			name: "delta before start",
			events: []map[string]any{{
				"type": "content_block_delta", "index": 7,
				"delta": map[string]any{"type": "text_delta", "text": "orphaned"},
			}},
			want: "content delta references unopened block 7",
		},
		{
			name:   "stop before start",
			events: []map[string]any{{"type": "content_block_stop", "index": 7}},
			want:   "content stop references unopened block 7",
		},
		{
			name: "incomplete block",
			events: []map[string]any{
				{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}},
				{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}},
				{"type": "message_stop"},
			},
			want: "returned an incomplete text block",
		},
		{
			name: "invalid tool arguments",
			events: []map[string]any{
				{
					"type": "content_block_start", "index": 0,
					"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read", "input": []any{}},
				},
				{"type": "content_block_stop", "index": 0},
				{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}},
				{"type": "message_stop"},
			},
			want: "returned invalid arguments for tool \"read\"",
		},
		{
			name:   "missing stop reason",
			events: []map[string]any{{"type": "message_stop"}},
			want:   "stream ended without a stop reason",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				for _, event := range test.events {
					writeSSEEvent(t, writer, event)
				}
			}))
			backend := newTestServerBackend(t, server, "")
			_, err := backend.Complete(context.Background(), model.Request{}, nil)
			if !errors.Is(err, model.ErrProviderFailure) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want provider failure containing %q", err, test.want)
			}
		})
	}
}

func TestBackendUsesStableReplayIdentity(t *testing.T) {
	backend, err := New(Config{
		Provider: "test", Model: "display-model", APIModel: "wire-model",
		BaseURL: "https://user:password@example.test/v1/?token=secret#fragment", Authorizer: APIKey("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.backendID() != BackendID("test") || backend.backendID() != "anthropic_messages.test" {
		t.Fatalf("backend ID = %q", backend.backendID())
	}
}

func TestBuildRequestUsesDefaultMaxTokens(t *testing.T) {
	backend, err := New(Config{
		Provider: "test", Model: "display-model", APIModel: "wire-model",
		BaseURL: "http://example.invalid", Authorizer: APIKey("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(model.Request{})
	if err != nil || request.Model != "wire-model" || request.MaxTokens != defaultMaxTokens {
		t.Fatalf("wire request/error = %#v/%v", request, err)
	}
}

func TestBuildRequestPlacesOnePromptCacheBreakpointWhenEnabled(t *testing.T) {
	items := []model.Item{
		{Kind: model.ItemUserText, Text: "read the file"},
		{Kind: model.ItemAssistantText, ResponseID: "response_1", Text: "checking"},
		{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{
			ID: "call_1", Name: "read_file", RawArguments: `{"path":"README.md"}`,
		}},
		{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call_1", Content: model.TextContent("contents")}},
	}

	plain, err := plainRequest(newTestBackend(t, "http://example.invalid/v1"), items)
	if err != nil {
		t.Fatal(err)
	}
	if breakpoints(plain) != 0 {
		t.Fatalf("unrequested cache breakpoints: %#v", plain.Messages)
	}

	backend, err := New(Config{
		Provider: "test", Model: "test-model", MaxTokens: 1024, PromptCache: true,
		BaseURL: "http://example.invalid/v1", Authorizer: APIKey("secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := plainRequest(backend, items)
	if err != nil {
		t.Fatal(err)
	}
	// The protocol caps breakpoints per request, so exactly one must land, and it
	// must be last: it caches every earlier block including tools and system.
	if breakpoints(cached) != 1 {
		t.Fatalf("cache breakpoints = %d, want 1", breakpoints(cached))
	}
	blocks := cached.Messages[len(cached.Messages)-1].Content
	final := blocks[len(blocks)-1]
	if final.Type != "tool_result" || final.CacheControl == nil || final.CacheControl.Type != "ephemeral" {
		t.Fatalf("final block = %#v", final)
	}

	// An empty turn has nothing to anchor a breakpoint to.
	empty, err := backend.buildRequest(model.Request{})
	if err != nil || breakpoints(empty) != 0 {
		t.Fatalf("empty request/error = %#v/%v", empty, err)
	}
}

func plainRequest(backend *Backend, items []model.Item) (messagesRequest, error) {
	return backend.buildRequest(model.Request{Instructions: "instructions", Items: items})
}

func breakpoints(request messagesRequest) int {
	count := 0
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.CacheControl != nil {
				count++
			}
		}
	}
	return count
}

func TestNewRejectsNegativeMaxTokens(t *testing.T) {
	if _, err := New(Config{
		Provider: "test", Model: "model", MaxTokens: -1,
		BaseURL: "http://example.invalid", Authorizer: APIKey("unused"),
	}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("negative max tokens error = %v", err)
	}
}

func newTestBackend(t *testing.T, baseURL string) *Backend {
	return newTestBackendWithClient(t, baseURL, nil)
}

func newTestBackendWithClient(t *testing.T, baseURL string, client *http.Client) *Backend {
	t.Helper()
	backend, err := New(Config{
		Provider: "test", Model: "test-model", APIModel: "wire-model", MaxTokens: 131_072,
		BaseURL: baseURL, HTTPClient: client, Authorizer: APIKey("secret"), Header: http.Header{"X-Test": []string{"yes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func newTestServerBackend(t *testing.T, server *httptest.Server, path string) *Backend {
	t.Helper()
	client := server.Client()
	return newTestBackendWithClient(t, server.URL+path, client)
}

func writeSSEEvent(t *testing.T, writer io.Writer, value any) {
	t.Helper()
	data := mustJSON(t, value)
	if _, err := fmt.Fprintf(writer, "event: ignored-by-parser\ndata: %s\n\n", data); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Messages may add stop reasons under its versioning contract. An unknown one
// cannot be read as a finished answer.
func TestCompleteRejectsUnknownStopReason(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "handed_off"},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "message_stop"})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	var providerErr *model.ProviderError
	if !errors.Is(err, model.ErrProviderFailure) || !errors.As(err, &providerErr) || providerErr.Retryable ||
		!strings.Contains(err.Error(), "handed_off") {
		t.Fatalf("unknown stop reason error = %v / %#v", err, providerErr)
	}
}

func TestNormalizedStopReasonsHaveExpectedCompletionClassification(t *testing.T) {
	for provider, normalized := range stopReasons {
		wantIncomplete := normalized != "stop" && normalized != "tool_calls"
		if got := model.IsIncompleteStopReason(normalized); got != wantIncomplete {
			t.Errorf("stop reason %q normalized to %q: incomplete = %v, want %v", provider, normalized, got, wantIncomplete)
		}
	}
}
