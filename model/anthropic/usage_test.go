package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestCompleteKeepsCumulativeUsageAndCacheWritesOnStreamFailure(t *testing.T) {
	for _, status := range []agent.UsageStatus{agent.UsagePartial, agent.UsageFinal} {
		t.Run(string(status), func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("Request-Id", "req-1")
				_, _ = io.WriteString(writer, `data: {"type":"message_start","message":{"id":"msg-1","model":"actual-model","usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":4,"cache_creation_input_tokens":3}}}`+"\n\n")
				_, _ = io.WriteString(writer, `data: {"type":"message_delta","usage":{"output_tokens":5}}`+"\n\n")
				if status == agent.UsageFinal {
					_, _ = io.WriteString(writer, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`+"\n\n")
					_, _ = io.WriteString(writer, `data: {"type":"error","error":{"type":"overloaded_error","message":"lost"}}`+"\n\n")
				}
			}))
			response, err := newTestServerBackend(t, server, "").Complete(t.Context(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
			if err == nil {
				t.Fatal("expected stream failure")
			}
			output := 5
			if status == agent.UsageFinal {
				output = 9
			}
			details := response.UsageDetails
			if details.Status != status || details.RequestID != "req-1" || details.ResponseID != "msg-1" || details.Model != "actual-model" ||
				response.Usage != (agent.ModelUsage{InputTokens: 19, CachedInputTokens: 4, CacheWriteInputTokens: 3, OutputTokens: output, TotalTokens: 19 + output}) {
				t.Fatalf("usage = %#v, counts = %#v", details, response.Usage)
			}
		})
	}
}
