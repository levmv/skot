package responses

import (
	"bytes"
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

func TestCompleteStreamsAndPreservesEncryptedReasoning(t *testing.T) {
	reasoningRaw := jsontext.Value(`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"checking "}],"encrypted_content":"ciphertext"}`)
	messageRaw := jsontext.Value(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"hello"}]}`)
	var received struct {
		Model        string           `json:"model"`
		Instructions string           `json:"instructions"`
		Input        []jsontext.Value `json:"input"`
		Store        *bool            `json:"store"`
		Stream       bool             `json:"stream"`
	}
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{"type": "response.reasoning_summary_text.delta", "delta": "checking "})
		writeSSEEvent(t, writer, map[string]any{"type": "response.output_text.delta", "delta": "hel"})
		writeSSEEvent(t, writer, map[string]any{"type": "response.output_text.delta", "delta": "lo"})
		writeSSEEvent(t, writer, map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp_1", "status": "completed", "output": []jsontext.Value{reasoningRaw, messageRaw},
				"usage": map[string]any{
					"input_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 4, "cache_write_tokens": 3},
					"output_tokens": 5, "output_tokens_details": map[string]any{"reasoning_tokens": 3},
					"total_tokens": 17,
				},
			},
		})
	}))

	backend := newTestServerBackend(t, server, "/v1")
	backend.header = make(http.Header)
	backend.header.Set("X-Test", "yes")
	var events []model.StreamEvent
	response, err := backend.Complete(context.Background(), model.Request{
		Instructions: "be brief", Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}},
	}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "test-model" || !received.Stream || received.Store == nil || *received.Store {
		t.Fatalf("request = %#v", received)
	}
	if received.Instructions != "be brief" || len(received.Input) != 1 {
		t.Fatalf("request input = %#v", received)
	}
	var user struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(received.Input[0], &user); err != nil || user.Role != "user" || user.Content != "hi" {
		t.Fatalf("user input = %#v, %v", user, err)
	}
	if response.StopReason != "stop" || response.Usage.Tokens.Known() != (model.TokenCounts{
		InputTokens: 12, CachedInputTokens: 4, CacheWriteInputTokens: 3, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 17,
	}) {
		t.Fatalf("response metadata = %#v", response)
	}
	if len(response.Items) != 2 || response.Items[0].Kind != model.ItemReasoning || response.Items[0].Text != "checking " ||
		len(response.Items[0].ProviderData) != 1 || response.Items[0].ProviderData[0].Kind != reasoningItemDataKind ||
		string(response.Items[0].ProviderData[0].Data) != `{"id":"rs_1","encrypted_content":"ciphertext"}` ||
		bytes.Contains(response.Items[0].ProviderData[0].Data, []byte("checking")) ||
		response.Items[1].Kind != model.ItemAssistantText || response.Items[1].Text != "hello" || response.Items[1].Phase != "final_answer" {
		t.Fatalf("response items = %#v", response.Items)
	}
	if !reflect.DeepEqual(events, []model.StreamEvent{
		{Kind: model.EventReasoningSummaryDelta, Text: "checking "},
		{Kind: model.EventTextDelta, Text: "hel"},
		{Kind: model.EventTextDelta, Text: "lo"},
	}) {
		t.Fatalf("events = %#v", events)
	}
}

func TestCompleteAssemblesFinishedItemsWithCompactTerminalResponse(t *testing.T) {
	output := []jsontext.Value{
		jsontext.Value(`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"checking"}],"encrypted_content":"ciphertext"}`),
		jsontext.Value(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Reading."}]}`),
		jsontext.Value(`{"id":"fc_1","type":"function_call","call_id":"provider_call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}","status":"completed"}`),
	}
	for _, test := range []struct {
		name, terminal, status string
		terminalOutput         []jsontext.Value
	}{
		{name: "omitted output", terminal: "response.completed", status: "completed"},
		{name: "empty output", terminal: "response.completed", status: "completed", terminalOutput: []jsontext.Value{}},
		{name: "done alias", terminal: "response.done", status: "completed"},
		{name: "repeated output", terminal: "response.completed", status: "completed", terminalOutput: output},
		{name: "incomplete output", terminal: "response.incomplete", status: "incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writeSSEEvent(t, writer, map[string]any{"type": "response.reasoning_summary_text.delta", "delta": "checking"})
				writeSSEEvent(t, writer, map[string]any{"type": "response.output_text.delta", "delta": "Reading."})
				// Parallel output can finish in a different order than its indices.
				for _, index := range []int{2, 0, 1} {
					writeSSEEvent(t, writer, map[string]any{
						"type": "response.output_item.done", "output_index": index, "item": output[index],
					})
				}
				terminal := map[string]any{
					"id": "resp_1", "status": test.status,
					"usage": map[string]any{
						"input_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 4},
						"output_tokens": 5, "output_tokens_details": map[string]any{"reasoning_tokens": 3}, "total_tokens": 17,
					},
				}
				if test.terminalOutput != nil {
					terminal["output"] = test.terminalOutput
				}
				if test.status == "incomplete" {
					terminal["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				}
				writeSSEEvent(t, writer, map[string]any{"type": test.terminal, "response": terminal})
			}))
			backend := newTestServerBackend(t, server, "")
			var events []model.StreamEvent
			response, err := backend.Complete(t.Context(), model.Request{}, func(event model.StreamEvent) { events = append(events, event) })
			if err != nil {
				t.Fatal(err)
			}
			if response.Usage.Tokens.Known() != (model.TokenCounts{InputTokens: 12, CachedInputTokens: 4, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 17}) {
				t.Fatalf("usage = %#v", response.Usage)
			}
			if !reflect.DeepEqual(events, []model.StreamEvent{
				{Kind: model.EventReasoningSummaryDelta, Text: "checking"}, {Kind: model.EventTextDelta, Text: "Reading."},
			}) {
				t.Fatalf("duplicated or missing streaming output: %#v", events)
			}
			wantCount, wantStop := 3, "tool_calls"
			if test.status == "incomplete" {
				wantCount, wantStop = 2, "max_output_tokens"
			}
			if len(response.Items) != wantCount || response.StopReason != wantStop {
				t.Fatalf("output count/stop = %d/%s", len(response.Items), response.StopReason)
			}
			if response.Items[0].Kind != model.ItemReasoning || response.Items[0].Text != "checking" || len(response.Items[0].ProviderData) != 1 || string(response.Items[0].ProviderData[0].Data) != `{"id":"rs_1","encrypted_content":"ciphertext"}` || response.Items[1].Kind != model.ItemAssistantText || response.Items[1].Text != "Reading." {
				t.Fatalf("lost or reordered completed items: %#v", response.Items)
			}
			if test.status == "completed" {
				call := response.Items[2].ToolCall
				if call == nil || call.Name != "read_file" || call.RawArguments != `{"path":"README.md"}` || len(call.ProviderReferences) != 1 {
					t.Fatalf("tool call = %#v", call)
				}
				var identity functionCallIdentity
				if err := json.Unmarshal(call.ProviderReferences[0].Data, &identity); err != nil || identity.ID != "fc_1" || identity.CallID != "provider_call_1" {
					t.Fatalf("tool replay identity = %#v, %v", identity, err)
				}
			}
		})
	}
}

func TestCompleteDoesNotPromoteUnfinishedStreamItems(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": jsontext.Value(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read","arguments":"{}","status":"in_progress"}`),
		})
		writeSSEEvent(t, writer, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(t.Context(), model.Request{}, nil)
	if !errors.Is(err, model.ErrProviderFailure) || !strings.Contains(err.Error(), "no output items") {
		t.Fatalf("unfinished tool call was accepted: %v", err)
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
	reasoningState := jsontext.Value(`{"id":"rs_1","encrypted_content":"ciphertext"}`)
	identityRaw := jsontext.Value(`{"id":"fc_1","call_id":"provider_call_1","status":"completed"}`)
	request, err := backend.buildRequest(model.Request{
		ProviderEpoch: "epoch_1", Instructions: "instructions", Summary: "older summary",
		Items: []model.Item{
			{Kind: model.ItemUserText, Text: "inspect"},
			{Kind: model.ItemReasoning, ResponseID: "response_1", Text: "sanitized summary", ProviderContext: &model.ProviderContext{Backend: "responses.openai", Epoch: "epoch_1"}, ProviderData: []model.ProviderData{{Kind: reasoningItemDataKind, Data: reasoningState}}},
			{Kind: model.ItemAssistantText, ResponseID: "response_1", Text: "calling tool", Phase: "commentary"},
			{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{
				ID: "skot_call_1", Name: "read_file", RawArguments: `{"path":"README.md"}`,
				ProviderReferences: []model.ProviderReference{{
					Kind: backend.callReferenceKind(), Backend: "responses.openai", Epoch: "epoch_1", Data: identityRaw,
				}},
			}},
			{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "skot_call_1", Content: model.TextContent("contents")}},
		},
		Tools: []model.ToolSpec{{Name: "read_file", Description: "Read a file", InputSchema: jsontext.Value(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "gpt-test" || request.Instructions == nil || *request.Instructions != "instructions" || request.Store || !request.Stream || len(request.Input) != 6 {
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
	if assistant.Role != "assistant" || assistant.Content != "calling tool" || assistant.Phase != "commentary" || assistant.Type != "" || assistant.Status != "" {
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
	request, err := backend.buildRequest(model.Request{Items: []model.Item{
		{Kind: model.ItemUserText, Text: "inspect"},
		{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{ID: "call_1", Name: "read", RawArguments: `{"path":"shot.jpg"}`}},
		{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call_1", Content: model.ImageToolContent("image metadata", model.ImageContent{
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
	request, err := backend.buildRequest(model.Request{Items: []model.Item{{
		Kind: model.ItemReasoning, ResponseID: "response_1",
		ProviderData: []model.ProviderData{{
			Kind: reasoningItemDataKind, Data: jsontext.Value(`{"id":"rs_1","encrypted_content":"ciphertext"}`),
		}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 1 {
		t.Fatalf("input = %#v", request.Input)
	}
	var fields map[string]jsontext.Value
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
		Output: []jsontext.Value{
			jsontext.Value(`{"id":"fc_1","type":"function_call","call_id":"provider_call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}","status":"completed"}`),
			jsontext.Value(`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"cannot comply"}]}`),
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

func TestParseResponsePreservesPartialOutputWithoutToolCalls(t *testing.T) {
	for _, test := range []struct {
		name, status string
		details      *incompleteDetail
	}{
		{name: "token limit", status: "incomplete", details: &incompleteDetail{Reason: "max_output_tokens"}},
		{name: "content filter", status: "incomplete", details: &incompleteDetail{Reason: "content_filter"}},
		{name: "malformed completed call", status: "completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newTestBackend(t, "http://example.invalid/v1")
			response, err := backend.parseResponse(wireResponse{
				Status: test.status, IncompleteDetails: test.details,
				Output: []jsontext.Value{
					jsontext.Value(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}`),
					jsontext.Value(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"first\"}"}`),
					jsontext.Value(`{"type":"function_call","id":"fc_2","call_id":"call_2","name":"read","arguments":"{\"path\":\"unfinished"}`),
				},
			})
			if test.status == "completed" {
				if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
					t.Fatalf("malformed completed call error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(response.Items, []model.Item{{Kind: model.ItemAssistantText, Text: "partial"}}) ||
				response.StopReason != test.details.Reason {
				t.Fatalf("incomplete response = %#v", response)
			}
		})
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
	if response.StopReason != "max_tokens" || !model.IsIncompleteStopReason(response.StopReason) {
		t.Fatalf("response = %#v", response)
	}
}

func TestCompleteClassifiesStreamRateLimit(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "response.failed", "response": map[string]any{
				"status": "failed", "error": map[string]any{"code": "rate_limit_exceeded", "message": "opaque provider detail"},
			},
		})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	var providerErr *model.ProviderError
	if !errors.Is(err, model.ErrProviderFailure) || !errors.As(err, &providerErr) ||
		providerErr.Kind != model.ProviderErrorRateLimit || providerErr.Code != "rate_limit_exceeded" || !providerErr.Retryable {
		t.Fatalf("error/metadata = %v / %#v", err, providerErr)
	}
}

func TestNormalizedIncompleteReasonsAreIncomplete(t *testing.T) {
	for provider, normalized := range incompleteReasons {
		if !model.IsIncompleteStopReason(normalized) {
			t.Errorf("incomplete reason %q normalized to %q is not incomplete", provider, normalized)
		}
	}
}

func TestCompleteRejectsMismatchedTerminalEventStatus(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSEEvent(t, writer, map[string]any{
			"type": "response.completed", "response": map[string]any{"status": "incomplete"},
		})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), `terminal event "response.completed" carries status "incomplete"`) {
		t.Fatalf("terminal mismatch error = %v", err)
	}
}

func TestParseResponseRequiresEncryptedReasoningState(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	_, err := backend.parseResponse(wireResponse{
		Status: "completed",
		Output: []jsontext.Value{jsontext.Value(`{"id":"rs_1","type":"reasoning","summary":[]}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "missing encrypted state") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildRequestDropsMismatchedProviderState(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	identityRaw := jsontext.Value(`{"id":"fc_old","call_id":"provider_old"}`)
	request, err := backend.buildRequest(model.Request{
		ProviderEpoch: "epoch_new",
		Items: []model.Item{
			{Kind: model.ItemReasoning, ResponseID: "response_1", ProviderContext: &model.ProviderContext{Backend: "responses.test", Epoch: "epoch_old"}, ProviderData: []model.ProviderData{{Kind: reasoningItemDataKind, Data: jsontext.Value(`{"id":"rs_old","encrypted_content":"old"}`)}}},
			{Kind: model.ItemToolCall, ResponseID: "response_1", ToolCall: &model.ToolCall{
				ID: "skot_call", Name: "read", RawArguments: `{}`,
				ProviderReferences: []model.ProviderReference{{Kind: backend.callReferenceKind(), Backend: "responses.test", Epoch: "epoch_old", Data: identityRaw}},
			}},
			{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "skot_call", Content: model.TextContent("done")}},
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
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(writer, "data: %s\n\ndata: %s\n\n", first, second)
	}))
	backend := newTestServerBackend(t, server, "")
	backend.maxCompletionBytes = len(first)
	var events []model.StreamEvent
	response, err := backend.Complete(context.Background(), model.Request{
		Items: []model.Item{{Kind: model.ItemUserText, Text: "continue"}},
	}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != model.StopReasonOutputLimit || !reflect.DeepEqual(response.Items, []model.Item{{Kind: model.ItemAssistantText, Text: "kept"}}) {
		t.Fatalf("partial response = %#v", response)
	}
	if !reflect.DeepEqual(events, []model.StreamEvent{{Kind: model.EventTextDelta, Text: "kept"}}) {
		t.Fatalf("events = %#v", events)
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

func TestCompleteReturnsStructuredProviderHTTPError(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "7")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"error":{"message":"slow down","type":"rate_limit"}}`)
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}}}, nil)
	var providerErr *model.ProviderError
	if !errors.Is(err, model.ErrProviderFailure) || !errors.As(err, &providerErr) ||
		providerErr.Kind != model.ProviderErrorRateLimit || !providerErr.Retryable ||
		providerErr.Type != "rate_limit" || providerErr.RetryAfter != 7*time.Second || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("error = %v / %#v", err, providerErr)
	}
}

func TestCompleteReturnsStructuredStreamErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		event    any
		want     string
		tooLarge bool
	}{
		{
			name: "failed response", want: "model failed",
			event: map[string]any{
				"type": "response.failed", "response": map[string]any{"error": map[string]any{"message": "model failed"}},
			},
		},
		{
			name: "error event", want: "bad request",
			event: map[string]any{"type": "error", "code": "invalid_request", "message": "bad request"},
		},
		{
			name: "context limit error event", want: "context rejected", tooLarge: true,
			event: map[string]any{"type": "error", "code": "context_length_exceeded", "message": "context rejected"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writeSSEEvent(t, writer, test.event)
			}))
			backend := newTestServerBackend(t, server, "")
			_, err := backend.Complete(context.Background(), model.Request{
				Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}},
			}, nil)
			if !errors.Is(err, model.ErrProviderFailure) || !strings.Contains(err.Error(), test.want) ||
				errors.Is(err, model.ErrModelRequestTooLarge) != test.tooLarge {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCompleteHonorsStreamIdleTimeout(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{
		Items: []model.Item{{Kind: model.ItemUserText, Text: "wait"}}, StreamIdleTimeout: 20 * time.Millisecond,
	}, nil)
	if !errors.Is(err, model.ErrModelStreamIdle) || !errors.Is(err, model.ErrProviderFailure) {
		t.Fatalf("idle error = %v", err)
	}
}

func TestCompleteHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	backend := newTestBackendWithClient(t, "http://example.invalid/v1", &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backend.Complete(ctx, model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "wait"}}}, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestCompleteRejectsStreamWithoutTerminalEvent(t *testing.T) {
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		writeSSEEvent(t, writer, map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": jsontext.Value(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read","arguments":"{}","status":"completed"}`),
		})
	}))
	backend := newTestServerBackend(t, server, "")
	_, err := backend.Complete(context.Background(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}}}, nil)
	if !errors.Is(err, model.ErrProviderFailure) || !strings.Contains(err.Error(), "terminal event") {
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
	request, err := backend.buildRequest(model.Request{})
	if err != nil || request.Model != "wire-model" || backend.backendID() != BackendID("openai") {
		t.Fatalf("wire model/backend/error = %q/%q/%v", request.Model, backend.backendID(), err)
	}
}

func newTestBackend(t *testing.T, baseURL string) *Backend {
	return newTestBackendWithClient(t, baseURL, nil)
}

func newTestBackendWithClient(t *testing.T, baseURL string, client *http.Client) *Backend {
	t.Helper()
	backend, err := New(Config{
		Provider: "test", Model: "test-model", BaseURL: baseURL,
		HTTPClient: client, Authorizer: BearerToken("secret"), Traits: RouteTraits{ReasoningSummary: ReasoningSummaryAuto},
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
		Output: []jsontext.Value{jsontext.Value(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "incomplete" || !model.IsIncompleteStopReason(response.StopReason) {
		t.Fatalf("response = %#v", response)
	}
}

func TestParseResponseRejectsUnknownIncompleteReason(t *testing.T) {
	backend := newTestBackend(t, "http://example.invalid/v1")
	_, err := backend.parseResponse(wireResponse{
		Status: "incomplete", IncompleteDetails: &incompleteDetail{Reason: "guardrail_intervened"},
		Output: []jsontext.Value{jsontext.Value(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}`)},
	})
	var providerErr *model.ProviderError
	if !errors.Is(err, model.ErrProviderFailure) || !errors.As(err, &providerErr) || providerErr.Retryable ||
		!strings.Contains(err.Error(), "guardrail_intervened") {
		t.Fatalf("unknown incomplete reason error = %v / %#v", err, providerErr)
	}
}
