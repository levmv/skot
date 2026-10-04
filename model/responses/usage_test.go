package responses

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levmv/skot/model"
)

func TestCompleteKeepsUsageAcrossFailedAndIncompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name, terminal string
		wantError      bool
		status         model.UsageStatus
		output         int
	}{
		{name: "failed", terminal: `{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":12,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3}},"error":{"message":"failed","code":"server_error"}}}`, wantError: true, status: model.UsageFinal, output: 5},
		{name: "invalid output", terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"read","arguments":"broken"}],"usage":{"input_tokens":12,"output_tokens":5}}}`, wantError: true, status: model.UsageFinal, output: 5},
		{name: "incomplete", terminal: `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":12,"output_tokens":5}}}`, status: model.UsageFinal, output: 5},
		{name: "interrupted", wantError: true, status: model.UsagePartial, output: 0},
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
			response, err := newTestServerBackend(t, server, "").Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hi"}}}, nil)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v", err)
			}
			details := response.Usage
			if details.Status != test.status || details.RequestID != "req-1" || details.ResponseID != "resp-1" || details.Model != "actual-model" ||
				response.Usage.Tokens.Known().InputTokens != 12 || response.Usage.Tokens.Known().CachedInputTokens != 4 || response.Usage.Tokens.Known().OutputTokens != test.output ||
				(details.Tokens.OutputTokens == nil) != (test.status == model.UsagePartial) {
				t.Fatalf("usage = %#v, counts = %#v", details, response.Usage)
			}
			if test.status == model.UsageFinal && response.Usage.Tokens.Known().TotalTokens != 12+test.output {
				t.Fatalf("total tokens = %d", response.Usage.Tokens.Known().TotalTokens)
			}
		})
	}
}
