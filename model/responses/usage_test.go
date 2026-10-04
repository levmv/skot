package responses

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levmv/skot/agent"
)

func TestCompleteKeepsUsageAcrossFailedAndIncompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name, terminal string
		wantError      bool
		status         agent.UsageStatus
		output         int
	}{
		{name: "failed", terminal: `{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":12,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3}},"error":{"message":"failed","code":"server_error"}}}`, wantError: true, status: agent.UsageFinal, output: 5},
		{name: "invalid output", terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"read","arguments":"broken"}],"usage":{"input_tokens":12,"output_tokens":5}}}`, wantError: true, status: agent.UsageFinal, output: 5},
		{name: "incomplete", terminal: `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":12,"output_tokens":5}}}`, status: agent.UsageFinal, output: 5},
		{name: "interrupted", wantError: true, status: agent.UsagePartial, output: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("X-Request-Id", "req-1")
				_, _ = io.WriteString(writer, `data: {"type":"response.created","response":{"id":"resp-1","model":"actual-model","usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":4}}}}`+"\n\n")
				if test.terminal != "" {
					_, _ = io.WriteString(writer, "data: "+test.terminal+"\n\n")
				}
			}))
			response, err := newTestServerBackend(t, server, "").Complete(t.Context(), agent.ModelRequest{Items: []agent.Item{{Kind: agent.ItemUserText, Text: "hi"}}}, nil)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v", err)
			}
			details := response.UsageDetails
			if details.Status != test.status || details.RequestID != "req-1" || details.ResponseID != "resp-1" || details.Model != "actual-model" ||
				response.Usage.InputTokens != 12 || response.Usage.CachedInputTokens != 4 || response.Usage.OutputTokens != test.output ||
				(details.Tokens.OutputTokens == nil) != (test.status == agent.UsagePartial) {
				t.Fatalf("usage = %#v, counts = %#v", details, response.Usage)
			}
			if test.status == agent.UsageFinal && response.Usage.TotalTokens != 12+test.output {
				t.Fatalf("total tokens = %d", response.Usage.TotalTokens)
			}
		})
	}
}
