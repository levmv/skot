package anthropic

import (
	"context"
	"encoding/json"
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

	"github.com/levmv/skot/agent"
)

func TestCompleteStreamsContentToolsAndUsage(t *testing.T) {
	var received messagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
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
	defer server.Close()

	backend := newTestBackend(t, server.URL+"/v1")
	var events []agent.ModelStreamEvent
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Instructions: "be brief", Summary: "prior turn",
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "read it"}},
		Tools: []agent.ToolSpec{{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, func(event agent.ModelStreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	t.Run("request", func(t *testing.T) {
		if received.Model != "wire-model" || received.MaxTokens != 131_072 || !received.Stream || received.System != "be brief\n\nConversation summary:\nprior turn" {
			t.Fatalf("request = %#v", received)
		}
		if got := received.Messages; !reflect.DeepEqual(got, []message{{
			Role: "user", Content: []contentBlock{{Type: "text", Text: "read it"}},
		}}) {
			t.Fatalf("messages = %#v", got)
		}
		if len(received.Tools) != 1 || received.Tools[0].Name != "read_file" || string(received.Tools[0].InputSchema) != `{"type":"object"}` {
			t.Fatalf("tools = %#v", received.Tools)
		}
	})
	t.Run("response", func(t *testing.T) {
		// Messages says tool_use; the agent sees the normalized reason.
		if response.StopReason != "tool_calls" || response.Usage != (agent.ModelUsage{
			InputTokens: 19, CachedInputTokens: 4, OutputTokens: 9, TotalTokens: 28,
		}) {
			t.Fatalf("stop/usage = %q/%#v", response.StopReason, response.Usage)
		}
		if len(response.Items) != 3 || response.Items[0].Kind != agent.ItemReasoning || response.Items[0].Text != "checking " ||
			response.Items[1].Kind != agent.ItemAssistantText || response.Items[1].Text != "hello" || response.Items[2].ToolCall == nil {
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
		if got := events; !reflect.DeepEqual(got, []agent.ModelStreamEvent{
			{Kind: agent.EventReasoningSummaryDelta, Text: "checking "},
			{Kind: agent.EventTextDelta, Text: "hello"},
		}) {
			t.Fatalf("events = %#v", got)
		}
	})
}

func TestBuildRequestMapsHistoryAndProviderToolID(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	providerID, _ := json.Marshal("toolu_provider_7")
	thinkingState, _ := json.Marshal(thinkingBlockState{Type: "thinking", Thinking: "original thinking", Signature: "signed-thinking"})
	request, err := backend.buildRequest(agent.ModelRequest{
		Instructions: "instructions", Summary: "summary", ProviderEpoch: "epoch_1",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "first"},
			{
				Kind: agent.ItemReasoning, ResponseID: "response_1", Text: "sanitized thinking",
				ProviderContext: &agent.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"},
				ProviderData:    []agent.ProviderData{{Kind: thinkingDataKind, Data: thinkingState}},
			},
			{Kind: agent.ItemAssistantText, ResponseID: "response_1", Text: "checking"},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID: "call_local", Name: "read_file", RawArguments: `{"path":"README.md"}`,
				ProviderReferences: []agent.ProviderReference{{
					Kind: "anthropic_messages.test.tool_use_id", Backend: "anthropic_messages.test", Epoch: "epoch_1", Data: providerID,
				}},
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_local", Content: "failed", Error: true}},
			{Kind: agent.ItemBoundaryText, Text: "Background job completed."},
			{Kind: agent.ItemUserText, Text: "continue"},
		},
		Tools: []agent.ToolSpec{{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}},
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
		user.Content[0].ToolUseID != "toolu_provider_7" || user.Content[0].Content == nil || *user.Content[0].Content != "failed" || !user.Content[0].IsError ||
		user.Content[1].Text != "Background job completed." || user.Content[2].Text != "continue" {
		t.Fatalf("user message = %#v", user)
	}
}

func TestBuildRequestPreservesEmptyThinkingAndToolResultFields(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	state, _ := json.Marshal(thinkingBlockState{Type: "thinking", Signature: "sig-abc"})
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_1",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "run the tool"},
			{
				Kind: agent.ItemReasoning, ResponseID: "response_1",
				ProviderContext: &agent.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"},
				ProviderData:    []agent.ProviderData{{Kind: thinkingDataKind, Data: state}},
			},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID: "call_1", Name: "empty", RawArguments: `{}`,
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_1"}},
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
			Content []map[string]json.RawMessage `json:"content"`
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
	state, _ := json.Marshal(thinkingBlockState{Type: "thinking", Signature: "signature"})
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_1",
		Items: []agent.Item{
			{
				Kind: agent.ItemReasoning, ResponseID: "response_1", Text: "private",
				ProviderContext: &agent.ProviderContext{Backend: "anthropic_messages.other", Epoch: "epoch_1"},
				ProviderData:    []agent.ProviderData{{Kind: thinkingDataKind, Data: state}},
			},
			{Kind: agent.ItemAssistantText, ResponseID: "response_1", Text: "visible"},
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
		0: {kind: "redacted_thinking", data: json.RawMessage(`"opaque-state"`), closed: true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != agent.ItemReasoning || items[0].Text != "" || len(items[0].ProviderData) != 1 {
		t.Fatalf("items = %#v", items)
	}
	items[0].ResponseID = "response_1"
	items[0].ProviderContext = &agent.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"}
	request, err := backend.buildRequest(agent.ModelRequest{ProviderEpoch: "epoch_1", Items: items})
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
	for _, data := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`123`), json.RawMessage(`""`)} {
		t.Run(string(data), func(t *testing.T) {
			items, err := backend.responseItems(map[int]*streamBlock{
				0: {kind: "redacted_thinking", data: data, closed: true},
			}, false)
			if err != nil || len(items) != 0 {
				t.Fatalf("items/error = %#v/%v", items, err)
			}

			state, err := json.Marshal(thinkingBlockState{Type: "redacted_thinking", Data: data})
			if err != nil {
				t.Fatal(err)
			}
			_, replayed, err := backend.replayThinkingBlock(agent.Item{
				ProviderContext: &agent.ProviderContext{Backend: "anthropic_messages.test", Epoch: "epoch_1"},
				ProviderData:    []agent.ProviderData{{Kind: thinkingDataKind, Data: state}},
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
	items, err := backend.responseItems(blocks, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(items, []agent.Item{
		{Kind: agent.ItemAssistantText, Text: "early"},
		{Kind: agent.ItemAssistantText, Text: "late"},
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: %s\n\ndata: %s\n\ndata: %s\n\n", start, kept, dropped)
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	backend.maxCompletionBytes = len(start) + len(kept)
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "continue"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != agent.StopReasonOutputLimit || !reflect.DeepEqual(response.Items, []agent.Item{{Kind: agent.ItemAssistantText, Text: "kept"}}) {
		t.Fatalf("response = %#v", response)
	}
}

func TestCompleteRejectsOversizedRequestWithoutSendingIt(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	backend.maxRequestBytes = 64
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: strings.Repeat("x", 128)}},
	}, nil)
	if !errors.Is(err, agent.ErrInvalidRequest) || requests.Load() != 0 {
		t.Fatalf("error/requests = %v/%d", err, requests.Load())
	}
}

func TestCompleteDecodesStructuredHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "2")
		writer.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(writer, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorAuthentication ||
		providerErr.Code != "" || providerErr.Type != "authentication_error" ||
		providerErr.StatusCode != http.StatusUnauthorized || providerErr.RetryAfter != 2*time.Second {
		t.Fatalf("error = %#v (%v)", providerErr, err)
	}
}

func TestCompleteReturnsStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "overloaded"},
		})
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
	if !errors.Is(err, agent.ErrProviderFailure) || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteReturnsEmptyRefusalForRuntimeClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal"},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "message_stop"})
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	response, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "refusal" || len(response.Items) != 0 {
		t.Fatalf("response = %#v", response)
	}
}

func TestCompleteHonorsStreamIdleTimeout(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{StreamIdleTimeout: 10 * time.Millisecond}, nil)
	if !errors.Is(err, agent.ErrModelStreamIdle) {
		t.Fatalf("error = %v", err)
	}
	<-started
}

func TestCompleteHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backend.Complete(ctx, agent.ModelRequest{}, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteRejectsStreamWithoutTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
	if !errors.Is(err, agent.ErrProviderFailure) || !strings.Contains(err.Error(), "before message_stop") {
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
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				for _, event := range test.events {
					writeSSEEvent(t, writer, event)
				}
			}))
			defer server.Close()
			backend := newTestBackend(t, server.URL)
			_, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
			if !errors.Is(err, agent.ErrProviderFailure) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want provider failure containing %q", err, test.want)
			}
		})
	}
}

func TestBuildRequestRejectsNonObjectToolJSON(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	for _, request := range []agent.ModelRequest{
		{Tools: []agent.ToolSpec{{Name: "bad", InputSchema: json.RawMessage(`true`)}}},
		{Items: []agent.Item{{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
			ID: "call_1", Name: "bad", RawArguments: `[]`,
		}}}},
	} {
		if _, err := backend.buildRequest(request); err == nil || !strings.Contains(err.Error(), "JSON object") {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestModelInfoReportsAnthropicIdentity(t *testing.T) {
	backend, err := New(Config{
		Provider: "test", Model: "display-model", APIModel: "wire-model",
		ContextWindow: 1_000_000, ContextWindowEstimated: true,
		BaseURL: "https://user:password@example.test/v1/?token=secret#fragment", Authorizer: APIKey("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	info := backend.Info()
	if info.Backend != "anthropic_messages.test" || info.Provider != "test" || info.Model != "display-model" ||
		info.ProviderStateContract != ProviderStateContract || info.Endpoint != "https://example.test/v1/" || !info.ContextWindowEstimated {
		t.Fatalf("model info = %#v", info)
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
	request, err := backend.buildRequest(agent.ModelRequest{})
	if err != nil || request.Model != "wire-model" || request.MaxTokens != defaultMaxTokens {
		t.Fatalf("wire request/error = %#v/%v", request, err)
	}
}

func TestNewRejectsNegativeMaxTokens(t *testing.T) {
	if _, err := New(Config{
		Provider: "test", Model: "model", MaxTokens: -1,
		BaseURL: "http://example.invalid", Authorizer: APIKey("unused"),
	}); !errors.Is(err, agent.ErrInvalidRequest) {
		t.Fatalf("negative max tokens error = %v", err)
	}
}

func newTestBackend(t *testing.T, baseURL string) *Backend {
	t.Helper()
	backend, err := New(Config{
		Provider: "test", Model: "test-model", APIModel: "wire-model", MaxTokens: 131_072,
		BaseURL: baseURL, Authorizer: APIKey("secret"), Header: http.Header{"X-Test": []string{"yes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "handed_off"},
		})
		writeSSEEvent(t, writer, map[string]any{"type": "message_stop"})
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrProviderFailure) || !errors.As(err, &providerErr) || providerErr.Retryable ||
		!strings.Contains(err.Error(), "handed_off") {
		t.Fatalf("unknown stop reason error = %v / %#v", err, providerErr)
	}
}
