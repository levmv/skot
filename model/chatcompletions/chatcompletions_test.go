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
	"sync/atomic"
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
		_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\r\n\r\n")
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
	if response.Usage != (agent.ModelUsage{
		InputTokens: 12, CachedInputTokens: 4, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 17,
	}) {
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
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
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
	if requests.Load() != 0 {
		t.Fatalf("provider requests = %d, want 0", requests.Load())
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
		Traits: RouteTraits{
			ReasoningReplay: ReasoningReplayToolTurns,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	owned := &agent.ProviderContext{Backend: "chat_completions.deepseek", Epoch: "epoch_deepseek"}
	items := []agent.Item{
		{Kind: agent.ItemUserText, Text: "first"},
		{Kind: agent.ItemReasoning, ResponseID: "response_1", ProviderContext: owned, Text: "tool reasoning"},
		{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
			ID: "call_1", Name: "read", RawArguments: `{"path":"README.md"}`,
		}},
		{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_1", Content: "contents"}},
		{Kind: agent.ItemReasoning, ResponseID: "response_2", ProviderContext: owned, Text: "plain reasoning"},
		{Kind: agent.ItemAssistantText, ResponseID: "response_2", Text: "done"},
		{Kind: agent.ItemUserText, Text: "second"},
	}
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_deepseek",
		Items:         items,
		Tools:         []agent.ToolSpec{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 5 {
		t.Fatalf("messages = %#v", request.Messages)
	}
	if request.Messages[1].ReasoningContent != "tool reasoning" || request.Messages[3].ReasoningContent != "" {
		t.Fatalf("deepseek reasoning projection = %#v", request.Messages)
	}
	if len(items) != 7 || items[4].Kind != agent.ItemReasoning || items[4].Text != "plain reasoning" {
		t.Fatalf("caller items were mutated: %#v", items)
	}
}

func TestBuildRequestUsesSessionAsOpenAIPromptCacheKey(t *testing.T) {
	backend, err := New(Config{
		Provider:   "openai",
		Model:      "gpt-test",
		BaseURL:    "http://example.invalid/v1",
		Authorizer: BearerToken("unused"),
		Traits:     RouteTraits{PromptCacheKey: true},
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

func TestBuildRequestDoesNotInferOptionalFieldsFromProviderName(t *testing.T) {
	backend, err := New(Config{
		Provider: "openai", Model: "gpt-test", BaseURL: "http://example.invalid/v1", Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(agent.ModelRequest{
		SessionID: "session_stable", Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.PromptCacheKey != "" {
		t.Fatalf("undeclared prompt cache key = %q", request.PromptCacheKey)
	}
	if _, err := New(Config{
		Provider: "openrouter", Model: "model", ReasoningEffort: "high",
		BaseURL: "http://example.invalid/v1", Authorizer: BearerToken("unused"),
	}); !errors.Is(err, agent.ErrInvalidRequest) {
		t.Fatalf("undeclared reasoning effort error = %v", err)
	}
}

func TestBuildRequestMapsReasoningEffortByRouteTrait(t *testing.T) {
	for _, test := range []struct {
		name         string
		effort       string
		encoding     ReasoningEffortEncoding
		wantTopLevel string
		wantNested   string
		wantThinking string
	}{
		{name: "top-level", effort: "high", encoding: ReasoningEffortTopLevel, wantTopLevel: "high"},
		{name: "nested", effort: "high", encoding: ReasoningEffortNested, wantNested: "high"},
		{name: "thinking off", effort: "off", encoding: ReasoningEffortThinking, wantThinking: "disabled"},
		{name: "thinking high", effort: "high", encoding: ReasoningEffortThinking, wantTopLevel: "high", wantThinking: "enabled"},
		{name: "thinking max", effort: "max", encoding: ReasoningEffortThinking, wantTopLevel: "max", wantThinking: "enabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, err := New(Config{
				Provider: "test", Model: "model", ReasoningEffort: " " + strings.ToUpper(test.effort) + " ", Traits: RouteTraits{ReasoningEffort: test.encoding},
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
			gotThinking := ""
			if request.Thinking != nil {
				gotThinking = request.Thinking.Type
			}
			if gotThinking != test.wantThinking {
				t.Fatalf("thinking = %q", gotThinking)
			}
			if backend.Info().ReasoningEffort != test.effort {
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
	if endpoint := PublicEndpoint("  https://other:secret@example.test/v1/?token=secret#fragment  "); endpoint != "https://example.test/v1/" {
		t.Fatalf("public endpoint = %q", endpoint)
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
	if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorRateLimit ||
		providerErr.Code != "" || providerErr.Type != "rate_limit" || !providerErr.Retryable || providerErr.RetryAfter != 7*time.Second {
		t.Fatalf("429 provider metadata = %#v", providerErr)
	}
}

func TestCompleteClassifiesDeepSeekQuotaCodeBeforeHTTP429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"message":"billing detail","type":"rate_limit_error","code":"insufficient_quota"}}`)
	}))
	defer server.Close()

	backend, err := New(Config{
		Provider: "deepseek", Model: "deepseek-v4-flash", BaseURL: server.URL,
		Authorizer: BearerToken("secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, nil)
	var providerErr *agent.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorQuota || providerErr.Retryable ||
		providerErr.Code != "insufficient_quota" || providerErr.Type != "rate_limit_error" {
		t.Fatalf("quota provider metadata = %#v (%v)", providerErr, err)
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
	if !errors.Is(err, agent.ErrProviderFailure) || errors.Is(err, agent.ErrInvalidRequest) ||
		!errors.As(err, &providerErr) || providerErr.Kind != agent.ProviderErrorQuota || providerErr.Retryable {
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

// Gateways such as OpenRouter answer a slow model with comment keep-alives
// only. They are stream activity, so the idle deadline must not fire while
// they arrive.
func TestCompleteKeepsStreamAliveThroughComments(t *testing.T) {
	interval := 10 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		flusher.Flush()
		for range 6 {
			select {
			case <-time.After(interval):
			case <-request.Context().Done():
				return
			}
			_, _ = io.WriteString(writer, ": OPENROUTER PROCESSING\n\n")
			flusher.Flush()
		}
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}, StreamIdleTimeout: 3 * interval,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].Text != "hi" || response.StopReason != "stop" {
		t.Fatalf("response = %#v", response)
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
		Traits: RouteTraits{
			ReasoningEffort: ReasoningEffortTopLevel,
			ReasoningReplay: ReasoningReplayCurrentTurn,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func projectionTestBackend(t *testing.T, policy ReasoningReplayPolicy) *Backend {
	t.Helper()
	backend, err := New(Config{
		Provider:   "deepseek",
		Model:      "deepseek-v4-flash",
		BaseURL:    "http://example.invalid/v1",
		Authorizer: BearerToken("unused"),
		Traits:     RouteTraits{ReasoningReplay: policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func projectionTestHistory() []agent.Item {
	owned := &agent.ProviderContext{Backend: "chat_completions.deepseek", Epoch: "epoch_deepseek"}
	return []agent.Item{
		{Kind: agent.ItemUserText, Text: "first"},
		{Kind: agent.ItemReasoning, ResponseID: "response_1", ProviderContext: owned, Text: "tool thinking"},
		{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{ID: "call_1", Name: "read", RawArguments: `{}`}},
		{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_1", Content: "contents"}},
		{Kind: agent.ItemReasoning, ResponseID: "response_2", ProviderContext: owned, Text: "plain thinking"},
		{Kind: agent.ItemAssistantText, ResponseID: "response_2", Text: "done"},
		{Kind: agent.ItemUserText, Text: "second"},
		{Kind: agent.ItemReasoning, ResponseID: "response_3", ProviderContext: owned, Text: "current thinking"},
		{Kind: agent.ItemAssistantText, ResponseID: "response_3", Text: "answer"},
	}
}

func TestProjectModelItemsAppliesRouteReplayPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy ReasoningReplayPolicy
		want   []string
	}{
		{name: "tool turns keep reasoning of tool-call turns only", policy: ReasoningReplayToolTurns, want: []string{"tool thinking"}},
		{name: "current turn keeps reasoning after the last user message", policy: ReasoningReplayCurrentTurn, want: []string{"current thinking"}},
		{name: "zero policy replays no reasoning", policy: "", want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			history := projectionTestHistory()
			projected := projectionTestBackend(t, test.policy).ProjectModelItems(append([]agent.Item(nil), history...))
			var got []string
			for _, item := range projected {
				if item.Kind == agent.ItemReasoning {
					got = append(got, item.Text)
				}
			}
			if len(got) != len(test.want) {
				t.Fatalf("replayed reasoning = %q, want %q", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("replayed reasoning = %q, want %q", got, test.want)
				}
			}
			if len(projected) != len(history)-(3-len(test.want)) {
				t.Fatalf("projected %d of %d items", len(projected), len(history))
			}
		})
	}
}

func TestProviderStateContractVersionsEachReplayPolicy(t *testing.T) {
	tests := []struct {
		policy ReasoningReplayPolicy
		want   agent.ProviderStateContract
	}{
		{policy: ReasoningReplayToolTurns, want: "chat_completions.reasoning_replay.tool_turns.v2"},
		{policy: ReasoningReplayCurrentTurn, want: "chat_completions.reasoning_replay.current_turn.v1"},
		{policy: "", want: ""},
	}
	for _, test := range tests {
		if got := (RouteTraits{ReasoningReplay: test.policy}).ProviderStateContract(); got != test.want {
			t.Fatalf("contract for %q = %q, want %q", test.policy, got, test.want)
		}
	}
}

// An uninterpretable completion reason must not pass for a finished answer. A
// retry would only fetch the same uninterpretable value.
func TestCompleteRejectsUnknownFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"guardrail_intervened\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, nil)
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrProviderFailure) || !errors.As(err, &providerErr) || providerErr.Retryable {
		t.Fatalf("unknown finish reason error = %v / %#v", err, providerErr)
	}
	if !strings.Contains(err.Error(), "guardrail_intervened") {
		t.Fatalf("error does not name the reason: %v", err)
	}
}

func TestCompleteNormalizesDoneOnlyTerminalToStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "stop" || len(response.Items) != 1 || response.Items[0].Text != "done" {
		t.Fatalf("response = %#v", response)
	}
}

func TestNormalizeFinishReasonCollapsesKnownSynonyms(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	tests := []struct{ raw, want string }{
		{raw: "stop", want: "stop"},
		{raw: "end_turn", want: "stop"},
		{raw: "TOOL_CALLS", want: "tool_calls"},
		{raw: "function_call", want: "tool_calls"},
		{raw: "length", want: "length"},
		{raw: "insufficient_system_resource", want: "insufficient_system_resource"},
	}
	for _, test := range tests {
		got, err := backend.normalizeFinishReason(test.raw)
		if err != nil || got != test.want {
			t.Fatalf("normalize(%q) = %q, %v; want %q", test.raw, got, err, test.want)
		}
	}
}

// A filtered or interrupted answer legitimately carries no content. Rejecting
// it would turn a deliberate incomplete result into a retried request failure.
func TestCompleteAcceptsEmptyIncompleteResponses(t *testing.T) {
	for _, reason := range []string{"content_filter", "insufficient_system_resource", "length"} {
		t.Run(reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", reason)
				_, _ = io.WriteString(writer, "data: [DONE]\n\n")
			}))
			defer server.Close()

			backend := newTestBackend(t, server.URL)
			response, err := backend.Complete(context.Background(), agent.ModelRequest{
				Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
			}, nil)
			if err != nil {
				t.Fatalf("empty %s response = %v", reason, err)
			}
			if len(response.Items) != 0 || response.StopReason != reason {
				t.Fatalf("response = %#v", response)
			}
			if !agent.IsIncompleteStopReason(response.StopReason) {
				t.Fatalf("%q is not an incomplete stop reason", response.StopReason)
			}
		})
	}
}

func TestCompleteRejectsEmptyFinishedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL)
	if _, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, nil); err == nil || !strings.Contains(err.Error(), "no output items") {
		t.Fatalf("empty finished response error = %v", err)
	}
}

// A declaration that offers "off" must also pick an encoding able to express
// it, otherwise the value would be sent as an effort the provider never defined.
func TestNewRejectsOffWithoutTheThinkingEncoding(t *testing.T) {
	_, err := New(Config{
		Provider: "deepseek", Model: "deepseek-v4-flash", BaseURL: "http://example.invalid/v1",
		Authorizer: BearerToken("unused"), ReasoningEffort: "off",
		Traits: RouteTraits{ReasoningEffort: ReasoningEffortTopLevel},
	})
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("error = %v", err)
	}
}
