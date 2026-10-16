package chatcompletions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/transport"
)

func TestCompleteDrainsParkedStreamsWithoutLosingUsageOrErrors(t *testing.T) {
	const partial = `{"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
	const answer = `{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`
	const usage = `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`
	for _, test := range []struct {
		name, before, after string
		failure             bool
		status              model.UsageStatus
		output              int
	}{
		{name: "late final usage", after: usage, status: model.UsageFinal, output: 3},
		{name: "missing usage", status: model.UsageUnavailable},
		{name: "partial stays partial", before: partial, status: model.UsagePartial, output: 2},
		{name: "error after final usage", after: usage, failure: true, status: model.UsageFinal, output: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					writer.Header().Set("Content-Type", "text/event-stream")
					if test.before != "" {
						_, _ = io.WriteString(writer, "data: "+test.before+"\n\n")
					}
					_, _ = io.WriteString(writer, "data: "+answer+"\n\n: ping\n: ping\n\n")
					writer.(http.Flusher).Flush()
					if test.after != "" {
						synctest.Sleep(4 * time.Second)
						_, _ = io.WriteString(writer, "data: "+test.after+"\n\n: ping\n: ping\n\n")
						writer.(http.Flusher).Flush()
					}
					if test.failure {
						synctest.Sleep(time.Second)
						_, _ = io.WriteString(writer, `data: {"error":{"code":429,"message":"busy"}}`+"\n\n")
						writer.(http.Flusher).Flush()
					}
					<-request.Context().Done()
				}))
				response, err := newTestServerBackend(t, server, "").Complete(t.Context(), model.Request{
					Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}},
				}, nil)
				if test.failure {
					var providerErr *model.ProviderError
					if !errors.As(err, &providerErr) || !strings.Contains(err.Error(), "busy") {
						t.Fatalf("trailing provider error = %v", err)
					}
				} else if err != nil || len(response.Items) != 1 || response.Items[0].Text != "answer" || response.StopReason != "stop" {
					t.Fatalf("completion = %#v, %v", response, err)
				}
				if response.Usage.Status != test.status || response.Usage.Tokens.Known().OutputTokens != test.output ||
					(response.Usage.Tokens.InputTokens == nil) != (test.status == model.UsageUnavailable) {
					t.Fatalf("usage = %#v", response.Usage)
				}
			})
		})
	}
}

func TestCompleteDistinguishesMissingZeroAndPartialUsage(t *testing.T) {
	const answer = `{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`
	for _, test := range []struct {
		name, before, after string
		status              model.UsageStatus
		known               bool
	}{
		{name: "missing", status: model.UsageUnavailable},
		{name: "null", after: `{"choices":[],"usage":null}`, status: model.UsageUnavailable},
		{name: "zero", after: `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, status: model.UsageFinal, known: true},
		{name: "earlier snapshot", before: `{"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, status: model.UsagePartial, known: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				for _, chunk := range []string{test.before, answer, test.after, "[DONE]"} {
					if chunk != "" {
						_, _ = io.WriteString(writer, "data: "+chunk+"\n\n")
					}
				}
			}))
			response, err := newTestServerBackend(t, server, "").Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			details := response.Usage
			if details.Status != test.status || (details.Tokens.InputTokens != nil) != test.known ||
				details.Tokens.CachedInputTokens != nil || len(details.Costs) != 0 {
				t.Fatalf("usage = %#v", details)
			}
			if details.Tokens.InputTokens != nil && *details.Tokens.InputTokens != 0 {
				t.Fatalf("input = %d", *details.Tokens.InputTokens)
			}
		})
	}
}

func TestCompleteKeepsOpenRouterReceiptWhenOutputIsRejected(t *testing.T) {
	const amount = "0.000000000000012345678900"
	for _, test := range []struct {
		name, byok string
		costCount  int
	}{
		{name: "credits", byok: `"is_byok":false,`, costCount: 1},
		{name: "byok", byok: `"is_byok":true,`, costCount: 2},
		{name: "unspecified", costCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("X-Request-Id", "req-1")
				_, _ = io.WriteString(writer, `data: {"id":"gen-1","model":"actual-model","provider":"upstream","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"broken"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
				_, _ = io.WriteString(writer, `data: {"choices":[],"usage":{`+test.byok+`"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":3},"cost":`+amount+`,"cost_details":{"upstream_inference_cost":0}}}`+"\n\ndata: [DONE]\n\n")
			}))
			backend, err := New(Config{
				Connection: newTestConnection(t, transport.Config{
					API: "chat_completions", Provider: "openrouter", APIModel: "test-model",
					ResponseProfile: model.ResponseProfileOpenRouter,
					Endpoint:        server.URL + "/chat/completions", HTTPClient: server.Client(), Authorizer: transport.BearerToken("secret"),
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err := backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}}}, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
				t.Fatalf("error = %v", err)
			}
			details := response.Usage
			if details.Status != model.UsageFinal || details.RequestID != "req-1" || details.ResponseID != "gen-1" || details.Model != "actual-model" || details.Provider != "upstream" ||
				response.Usage.Tokens.Known() != (model.TokenCounts{InputTokens: 12, CachedInputTokens: 4, CacheWriteInputTokens: 2, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 17}) {
				t.Fatalf("receipt = %#v, counts = %#v", details, response.Usage)
			}
			costs := make(map[string]model.ReportedCost)
			for _, cost := range details.Costs {
				costs[cost.Kind] = cost
			}
			if len(details.Costs) != test.costCount || costs["account_charge"] != (model.ReportedCost{Kind: "account_charge", Amount: amount, Currency: "USD"}) ||
				(test.costCount == 2 && costs["upstream_inference"] != (model.ReportedCost{Kind: "upstream_inference", Amount: "0", Currency: "USD"})) {
				t.Fatalf("costs = %#v", details.Costs)
			}
		})
	}
}

func TestCompleteKeepsPartialUsageOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":"partial"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`+"\n\n")
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	response, err := newTestServerBackend(t, server, "").Complete(ctx, model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}}}, func(model.StreamEvent) { cancel() })
	if err == nil || response.Usage.Status != model.UsagePartial || response.Usage.ResponseID != "gen-1" ||
		response.Usage.Tokens.Known() != (model.TokenCounts{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}) {
		t.Fatalf("cancelled response = %#v, %v", response, err)
	}
}
