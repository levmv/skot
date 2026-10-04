package chatcompletions

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestCompleteDistinguishesMissingZeroAndPartialUsage(t *testing.T) {
	const answer = `{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`
	for _, test := range []struct {
		name, before, after string
		status              agent.UsageStatus
		known               bool
	}{
		{name: "missing", status: agent.UsageUnavailable},
		{name: "null", after: `{"choices":[],"usage":null}`, status: agent.UsageUnavailable},
		{name: "zero", after: `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, status: agent.UsageFinal, known: true},
		{name: "earlier snapshot", before: `{"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, status: agent.UsagePartial, known: true},
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
			response, err := newTestServerBackend(t, server, "").Complete(t.Context(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			details := response.UsageDetails
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
			backend := newTestServerBackend(t, server, "")
			backend.provider = "openrouter"
			response, err := backend.Complete(t.Context(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
				t.Fatalf("error = %v", err)
			}
			details := response.UsageDetails
			if details.Status != agent.UsageFinal || details.RequestID != "req-1" || details.ResponseID != "gen-1" || details.Model != "actual-model" || details.Provider != "upstream" ||
				response.Usage != (agent.ModelUsage{InputTokens: 12, CachedInputTokens: 4, CacheWriteInputTokens: 2, OutputTokens: 5, ReasoningTokens: 3, TotalTokens: 17}) {
				t.Fatalf("receipt = %#v, counts = %#v", details, response.Usage)
			}
			costs := make(map[string]agent.ReportedCost)
			for _, cost := range details.Costs {
				costs[cost.Kind] = cost
			}
			if len(details.Costs) != test.costCount || costs["account_charge"] != (agent.ReportedCost{Kind: "account_charge", Amount: amount, Currency: "USD"}) ||
				(test.costCount == 2 && costs["upstream_inference"] != (agent.ReportedCost{Kind: "upstream_inference", Amount: "0", Currency: "USD"})) {
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
	response, err := newTestServerBackend(t, server, "").Complete(ctx, agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, func(agent.ModelStreamEvent) { cancel() })
	if err == nil || response.UsageDetails.Status != agent.UsagePartial || response.UsageDetails.ResponseID != "gen-1" ||
		response.Usage != (agent.ModelUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}) {
		t.Fatalf("cancelled response = %#v, %v", response, err)
	}
}
