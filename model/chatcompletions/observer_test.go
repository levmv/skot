package chatcompletions_test

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"net/http"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/model/transport"
)

func TestObserverUsageSnapshots(t *testing.T) {
	for _, test := range []struct {
		name          string
		chunks        []string
		status        model.UsageStatus
		known         bool
		input, output int
	}{
		{name: "absent", chunks: []string{`{"choices":[{"index":0,"finish_reason":"stop"}]}`}, status: model.UsageUnavailable},
		{name: "null", chunks: []string{`{"choices":[],"usage":null}`}, status: model.UsageUnavailable},
		{name: "zero", chunks: []string{`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`}, status: model.UsageFinal, known: true},
		{name: "partial stays partial", chunks: []string{`{"choices":[{"index":0}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`, `{"choices":[{"index":0,"finish_reason":"stop"}]}`}, status: model.UsagePartial, known: true, input: 2, output: 1},
		{name: "finish and usage together", chunks: []string{`{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{"tool_calls":[{"function":{"arguments":"{\"x\":"}}]}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`}, status: model.UsageFinal, known: true, input: 2, output: 1},
		{name: "replaced counters merged fields", chunks: []string{`{"choices":[{"index":0}],"usage":{"prompt_tokens":8,"completion_tokens":1}}`, `{"choices":[],"usage":{"completion_tokens":3}}`}, status: model.UsageFinal, known: true, input: 8, output: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newObserver(t)
			for _, chunk := range test.chunks {
				if err := observer.ObserveChunk([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			if err := observer.End(true); err != nil {
				t.Fatal(err)
			}
			usage := observer.Usage()
			if usage.Status != test.status || (usage.Tokens.InputTokens != nil) != test.known ||
				usage.Tokens.Known().InputTokens != test.input || usage.Tokens.Known().OutputTokens != test.output {
				t.Fatalf("usage = %#v", usage)
			}
			if !test.known {
				if observer.RawUsage() != nil {
					t.Fatalf("missing raw usage = %s", observer.RawUsage())
				}
				return
			}
			var raw struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			}
			if err := json.Unmarshal(observer.RawUsage(), &raw); err != nil || raw.Input != test.input || raw.Output != test.output {
				t.Fatalf("raw usage = %s, %v", observer.RawUsage(), err)
			}

		})
	}
}

func TestObserverRetainsUsageAndIdentifiersOnInBandError(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		kind         model.ProviderErrorKind
	}{
		{name: "top-level", fields: `"error":{"code":429,"message":"busy"},"choices":[]`, kind: model.ProviderErrorRateLimit},
		{name: "choice error", fields: `"choices":[{"index":0,"finish_reason":"error","error":{"code":401,"message":"denied"}}]`, kind: model.ProviderErrorAuthentication},
		{name: "finish error", fields: `"choices":[{"index":0,"finish_reason":"error"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newObserver(t)
			observer.ObserveHeader(http.Header{"X-Request-Id": {"req-primary"}, "Request-Id": {"req-fallback"}})
			body := []byte(`{"id":"response-1","model":"actual-model","provider":"upstream",` + test.fields + `,"usage":{"prompt_tokens":9,"completion_tokens":2,"cost":0.000000000000123400,"is_byok":true,"cost_details":{"upstream_inference_cost":0}}}`)
			err := observer.ObserveChunk(body)
			var providerErr *model.ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind != test.kind || !errors.Is(err, model.ErrProviderFailure) {
				t.Fatalf("error = %v", err)
			}
			usage := observer.Usage()
			if usage.Status != model.UsageFinal || usage.RequestID != "req-primary" || usage.ResponseID != "response-1" || usage.Model != "actual-model" || usage.Provider != "upstream" || usage.Tokens.Known().TotalTokens != 11 {
				t.Fatalf("usage = %#v", usage)
			}
			if len(usage.Costs) != 2 || usage.Costs[0].Amount != "0.000000000000123400" || usage.Costs[1].Amount != "0" {
				t.Fatalf("costs = %#v", usage.Costs)
			}
		})
	}
	observer := newObserver(t)
	observer.ObserveHeader(http.Header{"Request-Id": {"fallback"}})
	if observer.Usage().RequestID != "fallback" {
		t.Fatal("request-id fallback lost")
	}
}

func TestObserverCompletion(t *testing.T) {
	for _, test := range []struct {
		name, body                string
		response, done, wantError bool
		finish                    string
	}{
		{name: "done alone", done: true},
		{name: "early EOF", body: `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`, wantError: true},
		{name: "unknown reason", body: `{"choices":[{"index":0,"finish_reason":"vendor_reason"}]}`, finish: "vendor_reason"},
		{name: "json empty object", body: `{}`, response: true, wantError: true},
		{name: "json empty choices", body: `{"choices":[]}`, response: true, wantError: true},
		{name: "json no finish reason", body: `{"choices":[{"index":0,"message":{"role":"assistant","refusal":"no"}}]}`, response: true},
		{name: "json error envelope", body: `{"error":{"message":"unavailable"}}`, response: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newObserver(t)
			var err error
			if test.response {
				err = observer.ObserveResponse([]byte(test.body))
			} else {
				if test.body != "" {
					err = observer.ObserveChunk([]byte(test.body))
				}
				if err == nil {
					err = observer.End(test.done)
				}
			}
			if (err != nil) != test.wantError || observer.FinishReason() != test.finish || observer.Usage().Status != model.UsageUnavailable {
				t.Fatalf("error=%v finish=%q usage=%#v", err, observer.FinishReason(), observer.Usage())
			}
		})
	}
	observer := newObserver(t)
	if err := observer.ObserveResponse([]byte(`{"choices":[{"index":0}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)); err != nil || observer.Usage().Status != model.UsageFinal {
		t.Fatalf("JSON usage = %#v, %v", observer.Usage(), err)
	}
}

func TestObserverRetainsLastUsageAfterMalformedSnapshot(t *testing.T) {
	observer := newObserver(t)
	if err := observer.ObserveChunk([]byte(`{"choices":[{"index":0}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)); err != nil {
		t.Fatal(err)
	}
	raw := observer.RawUsage()
	if err := observer.ObserveChunk([]byte(`{"choices":[],"usage":{"completion_tokens":"invalid"}}`)); !errors.Is(err, model.ErrProviderFailure) {
		t.Fatalf("malformed usage error = %v", err)
	}
	if observer.Usage().Tokens.Known().TotalTokens != 6 || observer.Usage().Status != model.UsagePartial || !bytes.Equal(raw, observer.RawUsage()) {
		t.Fatalf("last valid usage = %#v, raw = %s", observer.Usage(), observer.RawUsage())
	}
}

func newObserver(t *testing.T) *chatcompletions.Observer {
	t.Helper()
	connection, err := transport.New(transport.Config{API: "chat_completions", Provider: "openrouter", APIModel: "test-model", Endpoint: "https://example.test/chat/completions", Authorizer: transport.BearerToken("unused")})
	if err != nil {
		t.Fatal(err)
	}
	return chatcompletions.NewObserver(connection)
}
