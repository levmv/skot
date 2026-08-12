package chatcompletions

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
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestCompleteStreamsTextAndReasoning(t *testing.T) {
	var received chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("X-Test") != "yes" {
			t.Errorf("X-Test = %q", request.Header.Get("X-Test"))
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, ": keep-alive\r\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"checking \"},\"finish_reason\":null}]}\r\n\r\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\r\n\r\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\r\n\r\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\r\n\r\n")
		_, _ = io.WriteString(writer, "data: [DONE]\r\n\r\n")
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL+"/v1")
	var events []agent.ModelStreamEvent
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Instructions: "be brief",
		Items:        []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, func(event agent.ModelStreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}

	if received.Model != "test-model" || !received.Stream {
		t.Fatalf("request model/stream = %q/%v", received.Model, received.Stream)
	}
	if received.StreamOptions == nil || !received.StreamOptions.IncludeUsage {
		t.Fatal("request does not ask for streaming usage")
	}
	if got := received.Messages; !reflect.DeepEqual(got, []chatMessage{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}) {
		t.Fatalf("messages = %#v", got)
	}
	if response.StopReason != "stop" {
		t.Fatalf("stop reason = %q", response.StopReason)
	}
	if response.Usage != (agent.ModelUsage{InputTokens: 12, CachedInputTokens: 4, OutputTokens: 5, TotalTokens: 17}) {
		t.Fatalf("usage = %#v", response.Usage)
	}
	if got := response.Items; !reflect.DeepEqual(got, []agent.Item{
		{Kind: agent.ItemReasoning, Text: "checking "},
		{Kind: agent.ItemAssistantText, Text: "hello"},
	}) {
		t.Fatalf("items = %#v", got)
	}
	if got := events; !reflect.DeepEqual(got, []agent.ModelStreamEvent{
		{Kind: agent.EventReasoningSummaryDelta, Text: "checking "},
		{Kind: agent.EventTextDelta, Text: "hel"},
		{Kind: agent.EventTextDelta, Text: "lo"},
	}) {
		t.Fatalf("events = %#v", got)
	}
}

func TestSSEReaderGrowsPastInitialBuffer(t *testing.T) {
	payload := strings.Repeat("x", initialSSEBufferBytes+1)
	reader := newSSEReader(strings.NewReader("data: " + payload + "\n\n"))
	got, err := reader.next()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("payload length = %d, want %d", len(got), len(payload))
	}
}

func TestStreamDeltaDecodesEitherReasoningField(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "standard", raw: `{"reasoning_content":"standard"}`, want: "standard"},
		{name: "alias", raw: `{"reasoning":"alias"}`, want: "alias"},
		{name: "standard wins", raw: `{"reasoning_content":"standard","reasoning":"alias"}`, want: "standard"},
		{name: "empty standard wins", raw: `{"reasoning_content":"","reasoning":"alias"}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var delta streamDelta
			if err := json.Unmarshal([]byte(test.raw), &delta); err != nil {
				t.Fatal(err)
			}
			if delta.ReasoningContent != test.want {
				t.Fatalf("reasoning = %q", delta.ReasoningContent)
			}
		})
	}
}

func TestCompleteAccumulatesToolCallDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"provider_call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"pa\"}}]},\"finish_reason\":null}]}\n\n")
		// Some compatible providers omit index on later deltas.
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"th\\\":\\\"README.md\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "read it"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].ToolCall == nil {
		t.Fatalf("items = %#v", response.Items)
	}
	call := response.Items[0].ToolCall
	if call.Name != "read_file" || call.RawArguments != `{"path":"README.md"}` {
		t.Fatalf("call = %#v", call)
	}
	if len(call.ProviderReferences) != 1 || call.ProviderReferences[0].Kind != "chat_completions.test.call_id" {
		t.Fatalf("provider references = %#v", call.ProviderReferences)
	}
	var providerID string
	if err := json.Unmarshal(call.ProviderReferences[0].Data, &providerID); err != nil || providerID != "provider_call_1" {
		t.Fatalf("provider call ID = %q, error = %v", providerID, err)
	}
}

func TestCompletePreservesPartialTextAtLocalOutputLimit(t *testing.T) {
	first := `{"choices":[{"index":0,"delta":{"content":"kept","tool_calls":[{"index":0,"id":"provider_call","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}]}`
	second := `{"choices":[{"index":0,"delta":{"content":"dropped"},"finish_reason":"stop"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, second)
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	backend.maxCompletionBytes = len(first)
	var events []agent.ModelStreamEvent
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "continue forever"}},
	}, func(event agent.ModelStreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != agent.StopReasonOutputLimit {
		t.Fatalf("stop reason = %q", response.StopReason)
	}
	if got := response.Items; !reflect.DeepEqual(got, []agent.Item{{Kind: agent.ItemAssistantText, Text: "kept"}}) {
		t.Fatalf("partial items = %#v", got)
	}
	if got := events; !reflect.DeepEqual(got, []agent.ModelStreamEvent{{Kind: agent.EventTextDelta, Text: "kept"}}) {
		t.Fatalf("stream events = %#v", got)
	}
}

func TestCompleteRejectsOversizedRequestWithoutSendingIt(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	backend.maxRequestBytes = 64
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: strings.Repeat("x", 128)}},
	}, nil)
	if !errors.Is(err, agent.ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("provider requests = %d, want 0", requests)
	}
}

func TestBuildRequestMapsProductCallIDBackToProviderID(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	providerID, _ := json.Marshal("provider_call_7")
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_1",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "read it"},
			{Kind: agent.ItemReasoning, ResponseID: "response_1", ProviderContext: &agent.ProviderContext{Backend: "chat_completions.test", Epoch: "epoch_1"}, Text: "need the file"},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID:           "call_local",
				Name:         "read_file",
				RawArguments: `{"path":"README.md"}`,
				ProviderReferences: []agent.ProviderReference{{
					Kind:    "chat_completions.test.call_id",
					Backend: "chat_completions.test",
					Epoch:   "epoch_1",
					Data:    providerID,
				}},
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_local", Content: "contents"}},
		},
		Tools: []agent.ToolSpec{{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 3 {
		t.Fatalf("messages = %#v", request.Messages)
	}
	assistantMessage := request.Messages[1]
	if assistantMessage.ReasoningContent != "need the file" || len(assistantMessage.ToolCalls) != 1 {
		t.Fatalf("assistant message = %#v", assistantMessage)
	}
	if assistantMessage.ToolCalls[0].ID != "provider_call_7" {
		t.Fatalf("assistant tool call ID = %q", assistantMessage.ToolCalls[0].ID)
	}
	if request.Messages[2].ToolCallID != "provider_call_7" {
		t.Fatalf("tool result call ID = %q", request.Messages[2].ToolCallID)
	}
	if len(request.Tools) != 1 || string(request.Tools[0].Function.Parameters) != `{"type":"object"}` {
		t.Fatalf("tools = %#v", request.Tools)
	}
}

func TestBuildRequestMapsBoundaryEventToSystemMessage(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	request, err := backend.buildRequest(agent.ModelRequest{Items: []agent.Item{
		{Kind: agent.ItemUserText, Text: "continue"},
		{Kind: agent.ItemBoundaryText, Text: "Background job job-1 completed."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := request.Messages; !reflect.DeepEqual(got, []chatMessage{
		{Role: "user", Content: "continue"},
		{Role: "system", Content: "Background job job-1 completed."},
	}) {
		t.Fatalf("messages = %#v", got)
	}
}

func TestBuildRequestStripsReasoningFromOlderTurns(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	request, err := backend.buildRequest(agent.ModelRequest{Items: []agent.Item{
		{Kind: agent.ItemUserText, Text: "first"},
		{Kind: agent.ItemReasoning, ResponseID: "response_1", Text: "old reasoning"},
		{Kind: agent.ItemAssistantText, ResponseID: "response_1", Text: "first answer"},
		{Kind: agent.ItemUserText, Text: "second"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 3 {
		t.Fatalf("messages = %#v", request.Messages)
	}
	if request.Messages[1].ReasoningContent != "" {
		t.Fatalf("old reasoning was sent: %#v", request.Messages[1])
	}
}

func TestBuildRequestKeepsDeepSeekReasoningFromToolTurns(t *testing.T) {
	backend, err := New(Config{
		Provider:   "deepseek",
		Model:      "deepseek-v4-flash",
		BaseURL:    "http://example.invalid/v1",
		Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_deepseek",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "first"},
			{Kind: agent.ItemReasoning, ResponseID: "response_1", ProviderContext: &agent.ProviderContext{Backend: "chat_completions.deepseek", Epoch: "epoch_deepseek"}, Text: "tool reasoning"},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID: "call_1", Name: "read", RawArguments: `{"path":"README.md"}`,
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_1", Content: "contents"}},
			{Kind: agent.ItemAssistantText, ResponseID: "response_2", Text: "done"},
			{Kind: agent.ItemUserText, Text: "second"},
		},
		Tools: []agent.ToolSpec{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 5 {
		t.Fatalf("messages = %#v", request.Messages)
	}
	if request.Messages[1].ReasoningContent != "tool reasoning" {
		t.Fatalf("deepseek reasoning was stripped: %#v", request.Messages[1])
	}
}

func TestBuildRequestUsesSessionAsOpenAIPromptCacheKey(t *testing.T) {
	backend, err := New(Config{
		Provider:   "openai",
		Model:      "gpt-test",
		BaseURL:    "http://example.invalid/v1",
		Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(agent.ModelRequest{
		SessionID: "session_stable",
		Items:     []agent.Item{{Kind: agent.ItemUserText, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.PromptCacheKey != "session_stable" {
		t.Fatalf("prompt cache key = %q", request.PromptCacheKey)
	}
}

func TestBuildRequestMapsReasoningEffortByProvider(t *testing.T) {
	for _, test := range []struct {
		provider     string
		wantTopLevel string
		wantNested   string
	}{
		{provider: "deepseek", wantTopLevel: "high"},
		{provider: "openai", wantTopLevel: "high"},
		{provider: "openrouter", wantNested: "high"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			backend, err := New(Config{
				Provider: test.provider, Model: "model", ReasoningEffort: " HIGH ",
				BaseURL: "http://example.invalid/v1", Authorizer: BearerToken("unused"),
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := backend.buildRequest(agent.ModelRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if request.ReasoningEffort != test.wantTopLevel {
				t.Fatalf("top-level effort = %q", request.ReasoningEffort)
			}
			gotNested := ""
			if request.Reasoning != nil {
				gotNested = request.Reasoning.Effort
			}
			if gotNested != test.wantNested {
				t.Fatalf("nested effort = %q", gotNested)
			}
			if backend.Info().ReasoningEffort != "high" {
				t.Fatalf("model info = %#v", backend.Info())
			}
		})
	}
}

func TestBuildRequestOmitsDefaultReasoningEffort(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	request, err := backend.buildRequest(agent.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reasoning_effort") || strings.Contains(string(raw), `"reasoning"`) {
		t.Fatalf("default effort changed wire request: %s", raw)
	}
}

func TestBuildRequestCanUseCanonicalAPIModelWithoutChangingSelection(t *testing.T) {
	backend, err := New(Config{
		Provider: "openrouter", Model: "free", APIModel: "openrouter/free",
		BaseURL: "http://example.invalid/v1", Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(agent.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "openrouter/free" || backend.Info().Model != "free" {
		t.Fatalf("wire/selection model = %q/%q", request.Model, backend.Info().Model)
	}
}

func TestModelInfoReportsSecretFreeEffectiveEndpoint(t *testing.T) {
	backend, err := New(Config{
		Provider: "test", Model: "model", ContextWindow: 64_000, ContextWindowEstimated: true,
		BaseURL: "https://user:password@example.test/v1?token=secret#fragment", Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	info := backend.Info()
	if info.Endpoint != "https://example.test/v1" || info.ContextWindow != 64_000 || !info.ContextWindowEstimated {
		t.Fatalf("model info = %#v", info)
	}
}

func TestBuildRequestPlacesSummaryBeforeVerbatimMessages(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	request, err := backend.buildRequest(agent.ModelRequest{
		Instructions: "instructions",
		Summary:      "older work summary",
		Items:        []agent.Item{{Kind: agent.ItemUserText, Text: "recent question"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 3 || request.Messages[0].Content != "instructions" ||
		request.Messages[1].Content != "Conversation summary:\nolder work summary" || request.Messages[2].Content != "recent question" {
		t.Fatalf("messages = %#v", request.Messages)
	}
}

func TestBuildRequestDropsMismatchedProviderMetadata(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	providerID, _ := json.Marshal("provider_call_old")
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_new",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "first"},
			{Kind: agent.ItemReasoning, ResponseID: "response_1", ProviderContext: &agent.ProviderContext{Backend: "chat_completions.test", Epoch: "epoch_old"}, Text: "old reasoning"},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID: "call_local", Name: "read", RawArguments: `{}`,
				ProviderReferences: []agent.ProviderReference{{
					Kind: "chat_completions.test.call_id", Backend: "chat_completions.test", Epoch: "epoch_old", Data: providerID,
				}},
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_local", Content: "done"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Messages[1].ReasoningContent != "" || request.Messages[1].ToolCalls[0].ID != "call_local" || request.Messages[2].ToolCallID != "call_local" {
		t.Fatalf("mismatched provider metadata leaked: %#v", request.Messages)
	}
}

func TestCompleteReturnsProviderHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "7")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"message":"slow down","type":"rate_limit"}}`)
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(err, agent.ErrProviderFailure) {
		t.Fatalf("429 class = %v", err)
	}
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) || !providerErr.Retryable || providerErr.RetryAfter != 7*time.Second {
		t.Fatalf("429 provider metadata = %#v", providerErr)
	}
}

func TestCompleteClassifiesPaymentRequiredAsNonRetryableProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(writer, `{"error":{"message":"more credits required"}}`)
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrProviderFailure) || errors.Is(err, agent.ErrInvalidRequest) || !errors.As(err, &providerErr) || providerErr.Retryable {
		t.Fatalf("402 class/metadata = %v / %#v", err, providerErr)
	}
}

func TestCompleteTimesOutIdleStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}, StreamIdleTimeout: 20 * time.Millisecond,
	}, nil)
	if !errors.Is(err, agent.ErrModelStreamIdle) || !errors.Is(err, agent.ErrProviderFailure) {
		t.Fatalf("idle stream error = %v", err)
	}
}

func TestCompleteClassifiesBadRequestAsNonRetryableProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error":{"message":"invalid messages"}}`)
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, nil)
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrProviderFailure) || errors.Is(err, agent.ErrInvalidRequest) || !errors.As(err, &providerErr) || providerErr.Retryable {
		t.Fatalf("400 class/metadata = %v / %#v", err, providerErr)
	}
}

func newTestBackend(t *testing.T, baseURL string) *Backend {
	t.Helper()
	header := make(http.Header)
	header.Set("X-Test", "yes")
	backend, err := New(Config{
		Provider:   "test",
		Model:      "test-model",
		BaseURL:    baseURL,
		Authorizer: BearerToken("secret"),
		Header:     header,
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}
