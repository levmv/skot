package responses

import (
	"bytes"
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

func TestCompleteStreamsAndPreservesEncryptedReasoning(t *testing.T) {
	reasoningRaw := json.RawMessage(`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"checking "}],"encrypted_content":"ciphertext"}`)
	messageRaw := json.RawMessage(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`)
	var received responseRequest
	var receivedRaw map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-Test") != "yes" {
			t.Errorf("headers = %#v", request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if err := json.Unmarshal(body, &receivedRaw); err != nil {
			t.Errorf("decode raw request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, streamEvent{Type: "response.reasoning_summary_text.delta", Delta: "checking "})
		writeSSEEvent(t, writer, streamEvent{Type: "response.output_text.delta", Delta: "hel"})
		writeSSEEvent(t, writer, streamEvent{Type: "response.output_text.delta", Delta: "lo"})
		writeSSEEvent(t, writer, streamEvent{Type: "response.completed", Response: &wireResponse{
			ID: "resp_1", Status: "completed", Output: []json.RawMessage{reasoningRaw, messageRaw},
			Usage: &responseUsage{
				InputTokens: 12, InputTokenDetails: &inputTokenDetails{CachedTokens: 4},
				OutputTokens: 5, OutputTokenDetails: &outputTokenDetails{ReasoningTokens: 3},
				TotalTokens: 17,
			},
		}})
	}))
	defer server.Close()

	backend := newTestBackend(t, server.URL+"/v1")
	backend.header = make(http.Header)
	backend.header.Set("X-Test", "yes")
	var events []agent.ModelStreamEvent
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Instructions: "be brief", Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
	}, func(event agent.ModelStreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "test-model" || !received.Stream || received.Store || string(receivedRaw["store"]) != "false" {
		t.Fatalf("request = %#v, raw store = %s", received, receivedRaw["store"])
	}
	if received.Instructions != "be brief" || len(received.Input) != 1 {
		t.Fatalf("request input = %#v", received)
	}
	var user inputMessage
	if err := json.Unmarshal(received.Input[0], &user); err != nil || user.Role != "user" || user.Content != "hi" {
		t.Fatalf("user input = %#v, %v", user, err)
	}
	if response.StopReason != "stop" || response.Usage != (agent.ModelUsage{
		InputTokens: 12, CachedInputTokens: 4, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 17,
	}) {
		t.Fatalf("response metadata = %#v", response)
	}
	if len(response.Items) != 2 || response.Items[0].Kind != agent.ItemReasoning || response.Items[0].Text != "checking " ||
		len(response.Items[0].ProviderData) != 1 || response.Items[0].ProviderData[0].Kind != reasoningItemDataKind ||
		string(response.Items[0].ProviderData[0].Data) != `{"id":"rs_1","encrypted_content":"ciphertext"}` ||
		bytes.Contains(response.Items[0].ProviderData[0].Data, []byte("checking")) ||
		response.Items[1].Kind != agent.ItemAssistantText || response.Items[1].Text != "hello" {
		t.Fatalf("response items = %#v", response.Items)
	}
	if !reflect.DeepEqual(events, []agent.ModelStreamEvent{
		{Kind: agent.EventReasoningSummaryDelta, Text: "checking "},
		{Kind: agent.EventTextDelta, Text: "hel"},
		{Kind: agent.EventTextDelta, Text: "lo"},
	}) {
		t.Fatalf("events = %#v", events)
	}
}

func TestBuildRequestReplaysOutputItemsAndToolIdentity(t *testing.T) {
	backend, err := New(Config{
		Provider: "openai", Model: "gpt-test", ReasoningEffort: " HIGH ",
		Traits:  RouteTraits{ReasoningSummary: ReasoningSummaryAuto},
		BaseURL: "http://example.invalid/v1", Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	reasoningState := json.RawMessage(`{"id":"rs_1","encrypted_content":"ciphertext"}`)
	identityRaw, _ := json.Marshal(functionCallIdentity{ID: "fc_1", CallID: "provider_call_1", Status: "completed"})
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_1", Instructions: "instructions", Summary: "older summary",
		Items: []agent.Item{
			{Kind: agent.ItemUserText, Text: "inspect"},
			{Kind: agent.ItemReasoning, ResponseID: "response_1", Text: "sanitized summary", ProviderContext: &agent.ProviderContext{Backend: "responses.openai", Epoch: "epoch_1"}, ProviderData: []agent.ProviderData{{Kind: reasoningItemDataKind, Data: reasoningState}}},
			{Kind: agent.ItemAssistantText, ResponseID: "response_1", Text: "calling tool"},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID: "skot_call_1", Name: "read_file", RawArguments: `{"path":"README.md"}`,
				ProviderReferences: []agent.ProviderReference{{
					Kind: backend.callReferenceKind(), Backend: "responses.openai", Epoch: "epoch_1", Data: identityRaw,
				}},
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "skot_call_1", Content: agent.TextContent("contents")}},
		},
		Tools: []agent.ToolSpec{{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "gpt-test" || request.Instructions != "instructions" || request.Store || !request.Stream || len(request.Input) != 6 {
		t.Fatalf("request = %#v", request)
	}
	if request.Reasoning == nil || request.Reasoning.Effort != "high" || request.Reasoning.Summary != ReasoningSummaryAuto {
		t.Fatalf("reasoning config = %#v", request.Reasoning)
	}
	var reasoning responseReasoningInput
	if err := json.Unmarshal(request.Input[2], &reasoning); err != nil {
		t.Fatal(err)
	}
	if reasoning.Type != "reasoning" || reasoning.ID != "rs_1" || reasoning.EncryptedContent != "ciphertext" ||
		!reflect.DeepEqual(reasoning.Summary, []responseSummaryPart{{Type: "summary_text", Text: "sanitized summary"}}) {
		t.Fatalf("replayed reasoning = %#v", reasoning)
	}
	var summary, user inputMessage
	if err := json.Unmarshal(request.Input[0], &summary); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(request.Input[1], &user); err != nil {
		t.Fatal(err)
	}
	if summary.Role != "developer" || summary.Content != "Conversation summary:\nolder summary" || user.Role != "user" || user.Content != "inspect" {
		t.Fatalf("summary/user = %#v / %#v", summary, user)
	}
	var call functionCallItem
	if err := json.Unmarshal(request.Input[4], &call); err != nil {
		t.Fatal(err)
	}
	var assistant inputMessage
	if err := json.Unmarshal(request.Input[3], &assistant); err != nil {
		t.Fatal(err)
	}
	var output functionCallOutputItem
	if err := json.Unmarshal(request.Input[5], &output); err != nil {
		t.Fatal(err)
	}
	if assistant.Role != "assistant" || assistant.Content != "calling tool" || assistant.Type != "" || assistant.Status != "" {
		t.Fatalf("assistant replay = %#v", assistant)
	}
	if call.ID != "fc_1" || call.CallID != "provider_call_1" || call.Name != "read_file" || call.Status != "completed" || output.CallID != "provider_call_1" || output.Output != "contents" {
		t.Fatalf("call/output = %#v / %#v", call, output)
	}
	if len(request.Tools) != 1 || request.Tools[0].Type != "function" || request.Tools[0].Strict || string(request.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("tools = %#v", request.Tools)
	}
}

func TestBuildRequestLowersImageFunctionOutput(t *testing.T) {
	backend, err := New(Config{
		Provider: "openai", Model: "gpt-test", BaseURL: "http://example.invalid/v1", Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(agent.ModelRequest{Items: []agent.Item{
		{Kind: agent.ItemUserText, Text: "inspect"},
		{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{ID: "call_1", Name: "read", RawArguments: `{"path":"shot.jpg"}`}},
		{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "call_1", Content: agent.ImageToolContent("image metadata", agent.ImageContent{
			MediaType: "image/jpeg", Data: []byte{1, 2, 3}, Width: 10, Height: 5,
		})}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Type   string                   `json:"type"`
		CallID string                   `json:"call_id"`
		Output []functionCallOutputPart `json:"output"`
	}
	if err := json.Unmarshal(request.Input[len(request.Input)-1], &output); err != nil {
		t.Fatal(err)
	}
	if output.Type != "function_call_output" || output.CallID != "call_1" || len(output.Output) != 2 ||
		output.Output[0].Type != "input_text" || output.Output[0].Text != "image metadata" ||
		output.Output[1].Type != "input_image" || output.Output[1].ImageURL != "data:image/jpeg;base64,AQID" {
		t.Fatalf("function output = %#v", output)
	}
}

func TestBuildRequestIncludesRequiredEmptyReasoningSummary(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	request, err := backend.buildRequest(agent.ModelRequest{Items: []agent.Item{{
		Kind: agent.ItemReasoning, ResponseID: "response_1",
		ProviderData: []agent.ProviderData{{
			Kind: reasoningItemDataKind, Data: json.RawMessage(`{"id":"rs_1","encrypted_content":"ciphertext"}`),
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 1 {
		t.Fatalf("input = %#v", request.Input)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(request.Input[0], &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["summary"]) != "[]" {
		t.Fatalf("reasoning input = %s", request.Input[0])
	}
}

func TestParseResponseMapsFunctionCallsAndRefusals(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	response, err := backend.parseResponse(wireResponse{
		Status: "completed",
		Output: []json.RawMessage{
			json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"provider_call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}","status":"completed"}`),
			json.RawMessage(`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"cannot comply"}]}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "tool_calls" || len(response.Items) != 2 || response.Items[0].ToolCall == nil || response.Items[1].Text != "cannot comply" {
		t.Fatalf("response = %#v", response)
	}
	call := response.Items[0].ToolCall
	if call.Name != "read_file" || call.RawArguments != `{"path":"README.md"}` || len(call.ProviderReferences) != 1 {
		t.Fatalf("tool call = %#v", call)
	}
	var identity functionCallIdentity
	if err := json.Unmarshal(call.ProviderReferences[0].Data, &identity); err != nil || identity.ID != "fc_1" || identity.CallID != "provider_call_1" {
		t.Fatalf("identity = %#v, %v", identity, err)
	}
}

func TestParseResponseMapsIncompleteStatusAndUsage(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	response, err := backend.parseResponse(wireResponse{
		Status: "incomplete", IncompleteDetails: &incompleteDetail{Reason: "max_output_tokens"},
		Output: []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}`)},
		Usage:  &responseUsage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "max_output_tokens" || response.Items[0].Text != "partial" || response.Usage.TotalTokens != 13 {
		t.Fatalf("response = %#v", response)
	}
}

func TestParseResponseAcceptsDocumentedIncompleteReasonAlias(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	response, err := backend.parseResponse(wireResponse{
		Status: "incomplete", IncompleteDetails: &incompleteDetail{Reason: "max_tokens"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "max_tokens" || !agent.IsIncompleteStopReason(response.StopReason) {
		t.Fatalf("response = %#v", response)
	}
}

func TestNormalizedIncompleteReasonsAreIncomplete(t *testing.T) {
	for provider, normalized := range incompleteReasons {
		if !agent.IsIncompleteStopReason(normalized) {
			t.Errorf("incomplete reason %q normalized to %q is not incomplete", provider, normalized)
		}
	}
}

func TestCompleteRejectsMismatchedTerminalEventStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, streamEvent{Type: "response.completed", Response: &wireResponse{Status: "incomplete"}})
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), `terminal event "response.completed" carries status "incomplete"`) {
		t.Fatalf("terminal mismatch error = %v", err)
	}
}

func TestParseResponseRequiresEncryptedReasoningState(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	_, err := backend.parseResponse(wireResponse{
		Status: "completed",
		Output: []json.RawMessage{json.RawMessage(`{"id":"rs_1","type":"reasoning","summary":[]}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "missing encrypted state") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildRequestDropsMismatchedProviderState(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	identityRaw, _ := json.Marshal(functionCallIdentity{ID: "fc_old", CallID: "provider_old"})
	request, err := backend.buildRequest(agent.ModelRequest{
		ProviderEpoch: "epoch_new",
		Items: []agent.Item{
			{Kind: agent.ItemReasoning, ResponseID: "response_1", ProviderContext: &agent.ProviderContext{Backend: "responses.test", Epoch: "epoch_old"}, ProviderData: []agent.ProviderData{{Kind: reasoningItemDataKind, Data: json.RawMessage(`{"id":"rs_old","encrypted_content":"old"}`)}}},
			{Kind: agent.ItemToolCall, ResponseID: "response_1", ToolCall: &agent.ToolCall{
				ID: "skot_call", Name: "read", RawArguments: `{}`,
				ProviderReferences: []agent.ProviderReference{{Kind: backend.callReferenceKind(), Backend: "responses.test", Epoch: "epoch_old", Data: identityRaw}},
			}},
			{Kind: agent.ItemToolResult, ToolResult: &agent.ToolResult{CallID: "skot_call", Content: agent.TextContent("done")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 2 {
		t.Fatalf("input = %#v", request.Input)
	}
	var call functionCallItem
	var output functionCallOutputItem
	if err := json.Unmarshal(request.Input[0], &call); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(request.Input[1], &output); err != nil {
		t.Fatal(err)
	}
	if call.ID != "" || call.CallID != "skot_call" || output.CallID != "skot_call" {
		t.Fatalf("mismatched state leaked: %#v / %#v", call, output)
	}
}

func TestCompletePreservesPartialTextAtLocalOutputLimit(t *testing.T) {
	first := `{"type":"response.output_text.delta","delta":"kept"}`
	second := `{"type":"response.output_text.delta","delta":"dropped"}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: %s\n\ndata: %s\n\n", first, second)
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	backend.maxCompletionBytes = len(first)
	var events []agent.ModelStreamEvent
	response, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "continue"}},
	}, func(event agent.ModelStreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != agent.StopReasonOutputLimit || !reflect.DeepEqual(response.Items, []agent.Item{{Kind: agent.ItemAssistantText, Text: "kept"}}) {
		t.Fatalf("partial response = %#v", response)
	}
	if !reflect.DeepEqual(events, []agent.ModelStreamEvent{{Kind: agent.EventTextDelta, Text: "kept"}}) {
		t.Fatalf("events = %#v", events)
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
	if !errors.Is(err, agent.ErrInvalidRequest) || !errors.Is(err, agent.ErrModelRequestTooLarge) || requests.Load() != 0 {
		t.Fatalf("error/requests = %v/%d", err, requests.Load())
	}
}

func TestCompleteReturnsStructuredProviderHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "7")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"message":"slow down","type":"rate_limit"}}`)
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrProviderFailure) || !errors.As(err, &providerErr) ||
		providerErr.Kind != agent.ProviderErrorRateLimit || !providerErr.Retryable ||
		providerErr.Type != "rate_limit" || providerErr.RetryAfter != 7*time.Second || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("error = %v / %#v", err, providerErr)
	}
}

func TestCompleteReturnsStructuredStreamErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		event    streamEvent
		want     string
		tooLarge bool
	}{
		{
			name: "failed response", want: "model failed",
			event: streamEvent{Type: "response.failed", Response: &wireResponse{Error: &apiError{Message: "model failed"}}},
		},
		{
			name: "error event", want: "bad request",
			event: streamEvent{Type: "error", Code: "invalid_request", Message: "bad request"},
		},
		{
			name: "context limit error event", want: "context rejected", tooLarge: true,
			event: streamEvent{Type: "error", Code: "context_length_exceeded", Message: "context rejected"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writeSSEEvent(t, writer, test.event)
			}))
			defer server.Close()
			backend := newTestBackend(t, server.URL)
			_, err := backend.Complete(context.Background(), agent.ModelRequest{
				Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}},
			}, nil)
			if !errors.Is(err, agent.ErrProviderFailure) || !strings.Contains(err.Error(), test.want) ||
				errors.Is(err, agent.ErrModelRequestTooLarge) != test.tooLarge {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCompleteHonorsStreamIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{
		Items: []agent.Item{{Kind: agent.ItemUserText, Text: "wait"}}, StreamIdleTimeout: 20 * time.Millisecond,
	}, nil)
	if !errors.Is(err, agent.ErrModelStreamIdle) || !errors.Is(err, agent.ErrProviderFailure) {
		t.Fatalf("idle error = %v", err)
	}
}

func TestCompleteHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	backend := newTestBackend(t, "http://example.invalid/v1")
	backend.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backend.Complete(ctx, agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "wait"}}}, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestCompleteRejectsStreamWithoutTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()
	backend := newTestBackend(t, server.URL)
	_, err := backend.Complete(context.Background(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
	if !errors.Is(err, agent.ErrProviderFailure) || !strings.Contains(err.Error(), "terminal event") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildRequestUsesCanonicalAPIModel(t *testing.T) {
	backend, err := New(Config{
		Provider: "openai", Model: "gpt-test", APIModel: "wire-model",
		BaseURL: "https://user:password@example.test/v1/?token=secret#fragment", Authorizer: BearerToken("unused"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := backend.buildRequest(agent.ModelRequest{})
	if err != nil || request.Model != "wire-model" || backend.backendID() != BackendID("openai") {
		t.Fatalf("wire model/backend/error = %q/%q/%v", request.Model, backend.backendID(), err)
	}
}

func newTestBackend(t *testing.T, baseURL string) *Backend {
	t.Helper()
	backend, err := New(Config{
		Provider: "test", Model: "test-model", BaseURL: baseURL,
		Authorizer: BearerToken("secret"), Traits: RouteTraits{ReasoningSummary: ReasoningSummaryAuto},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func writeSSEEvent(t *testing.T, writer io.Writer, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", data); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestParseResponseKeepsIncompleteStatusWithoutDetails(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	response, err := backend.parseResponse(wireResponse{
		Status: "incomplete",
		Output: []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "incomplete" || !agent.IsIncompleteStopReason(response.StopReason) {
		t.Fatalf("response = %#v", response)
	}
}

func TestParseResponseRejectsUnknownIncompleteReason(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	_, err := backend.parseResponse(wireResponse{
		Status: "incomplete", IncompleteDetails: &incompleteDetail{Reason: "guardrail_intervened"},
		Output: []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}`)},
	})
	var providerErr *agent.ProviderError
	if !errors.Is(err, agent.ErrProviderFailure) || !errors.As(err, &providerErr) || providerErr.Retryable ||
		!strings.Contains(err.Error(), "guardrail_intervened") {
		t.Fatalf("unknown incomplete reason error = %v / %#v", err, providerErr)
	}
}
