package google_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/google"
	"github.com/levmv/skot/model/transport"
)

func TestObserverRetainsMultiCandidateUsageOnCancellation(t *testing.T) {
	observer := newObserver(t, 2)
	observer.ObserveHeader(http.Header{"X-Request-Id": {"req-primary"}, "Request-Id": {"req-fallback"}})
	for _, chunk := range []string{
		`{"responseId":"response-1","modelVersion":"actual-model","candidates":[{"index":1,"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AQ=="},"thoughtSignature":"opaque"}]}}],"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":2,"cachedContentTokenCount":4,"candidatesTokenCount":1,"thoughtsTokenCount":1}}`,
		`{"candidates":[{"index":0,"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":3}}`,
	} {
		if err := observer.ObserveChunk([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if observer.Complete() || observer.Usage().Status != model.UsagePartial {
			t.Fatalf("early completion=%t usage=%+v", observer.Complete(), observer.Usage())
		}
	}
	if err := observer.ObserveChunk([]byte(`{"candidates":[{"index":1,"finishReason":"MAX_TOKENS"}]}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() || observer.FinishReason(0) != "STOP" || observer.FinishReason(1) != "MAX_TOKENS" || observer.Usage().Status != model.UsagePartial {
		t.Fatalf("completion=%t reasons=%q/%q usage=%+v", observer.Complete(), observer.FinishReason(0), observer.FinishReason(1), observer.Usage())
	}
	if err := observer.ObserveChunk([]byte(`{"usageMetadata":{"candidatesTokenCount":5,"thoughtsTokenCount":3,"futureUsage":{"tier":"x"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := observer.End(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("completion hid cancellation: %v", err)
	}
	usage := observer.Usage()
	want := model.TokenCounts{InputTokens: 12, OutputTokens: 8, ReasoningTokens: 3, CachedInputTokens: 4, TotalTokens: 20}
	if usage.Status != model.UsageFinal || usage.Tokens.Known() != want || usage.RequestID != "req-primary" || usage.ResponseID != "response-1" || usage.Model != "actual-model" {
		t.Fatalf("usage=%+v counts=%+v", usage, usage.Tokens.Known())
	}
	var raw struct {
		Prompt int `json:"promptTokenCount"`
		Output int `json:"candidatesTokenCount"`
		Future struct {
			Tier string `json:"tier"`
		} `json:"futureUsage"`
	}
	if err := json.Unmarshal(observer.RawUsage(), &raw); err != nil || raw.Prompt != 10 || raw.Output != 5 || raw.Future.Tier != "x" {
		t.Fatalf("raw usage=%s error=%v", observer.RawUsage(), err)
	}
}

func TestObserverCompletion(t *testing.T) {
	for _, test := range []struct {
		name, body, block, finish string
		candidateCount            int
		response, complete        bool
		readErr, wantErr          error
	}{
		{name: "unknown finish", body: `{"candidates":[{"finishReason":"FUTURE_STOP"}]}`, complete: true, finish: "FUTURE_STOP"},
		{name: "prompt block", body: `{"promptFeedback":{"blockReason":"SAFETY"}}`, complete: true, block: "SAFETY"},
		{name: "unspecified block", body: `{"promptFeedback":{"blockReason":"BLOCK_REASON_UNSPECIFIED"}}`, wantErr: io.ErrUnexpectedEOF},
		{name: "unspecified finish", body: `{"candidates":[{"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`, wantErr: io.ErrUnexpectedEOF},
		{name: "missing candidate", candidateCount: 2, body: `{"candidates":[{"index":0,"finishReason":"STOP"}]}`, wantErr: io.ErrUnexpectedEOF},
		{name: "unfinished candidate", candidateCount: 2, body: `{"candidates":[{"index":0,"finishReason":"STOP"},{"index":1,"content":{"parts":[{"text":"partial"}]}}]}`, wantErr: io.ErrUnexpectedEOF},
		{name: "unexpected index", body: `{"candidates":[{"index":1,"finishReason":"STOP"}]}`, wantErr: model.ErrProviderFailure},
		{name: "early EOF", body: `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, readErr: io.EOF, wantErr: io.ErrUnexpectedEOF},
		{name: "read failure after finish", body: `{"candidates":[{"finishReason":"STOP"}]}`, complete: true, readErr: io.ErrUnexpectedEOF, wantErr: io.ErrUnexpectedEOF},
		{name: "JSON complete", response: true, candidateCount: 2, body: `{"candidates":[{"index":1,"finishReason":"MAX_TOKENS"},{"index":0,"finishReason":"STOP"}]}`, complete: true},
		{name: "JSON blocked", response: true, body: `{"promptFeedback":{"blockReason":"SAFETY"}}`, complete: true, block: "SAFETY"},
		{name: "JSON unfinished", response: true, body: `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, wantErr: io.ErrUnexpectedEOF},
		{name: "empty JSON response", response: true, body: `{}`, wantErr: io.ErrUnexpectedEOF},
		{name: "not an object", response: true, body: `null`, wantErr: model.ErrProviderFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newObserver(t, test.candidateCount)
			var err error
			if test.response {
				err = observer.ObserveResponse([]byte(test.body))
			} else {
				err = observer.ObserveChunk([]byte(test.body))
				if err == nil {
					err = observer.End(test.readErr)
				}
			}
			if (test.wantErr == nil && err != nil) || (test.wantErr != nil && !errors.Is(err, test.wantErr)) {
				t.Fatalf("error=%v, want %v", err, test.wantErr)
			}
			if observer.Complete() != test.complete || observer.BlockReason() != test.block || (test.finish != "" && observer.FinishReason(0) != test.finish) {
				t.Fatalf("complete=%t block=%q finish=%q", observer.Complete(), observer.BlockReason(), observer.FinishReason(0))
			}
		})
	}
}

func TestObserverUsagePresence(t *testing.T) {
	for _, test := range []struct {
		name, field string
		status      model.UsageStatus
		tokens      model.TokenCounts
	}{
		{name: "missing", status: model.UsageUnavailable},
		{name: "null", field: `,"usageMetadata":null`, status: model.UsageUnavailable},
		{name: "zero", field: `,"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}`, status: model.UsageFinal},
		{name: "omitted zero", field: `,"usageMetadata":{}`, status: model.UsageFinal},
		{name: "thinking only", field: `,"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":5}`, status: model.UsageFinal, tokens: model.TokenCounts{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 5, TotalTokens: 15}},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newObserver(t, 1)
			if err := observer.ObserveResponse([]byte(`{"candidates":[{"finishReason":"STOP"}]` + test.field + `}`)); err != nil {
				t.Fatal(err)
			}
			usage := observer.Usage()
			known := usage.Tokens.InputTokens != nil && usage.Tokens.OutputTokens != nil
			if usage.Status != test.status || known != (test.status == model.UsageFinal) || usage.Tokens.Known() != test.tokens || (observer.RawUsage() != nil) != known {
				t.Fatalf("usage=%+v raw=%s", usage, observer.RawUsage())
			}
		})
	}
}

func TestObserverRetainsUsageOnErrors(t *testing.T) {
	for _, test := range []struct {
		name               string
		response, complete bool
		status             model.UsageStatus
	}{
		{name: "stream", status: model.UsagePartial},
		{name: "JSON", response: true, status: model.UsageFinal},
		{name: "error after finish", complete: true, status: model.UsageFinal},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := newObserver(t, 1)
			observer.ObserveHeader(http.Header{"Request-Id": {"req-fallback"}, "Retry-After": {"1"}})
			if test.complete {
				if err := observer.ObserveChunk([]byte(`{"candidates":[{"finishReason":"STOP"}]}`)); err != nil {
					t.Fatal(err)
				}
			}
			body := []byte(`{"responseId":"failed-response","modelVersion":"actual-model","error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"busy","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]},"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":0}}`)
			var err error
			if test.response {
				err = observer.ObserveResponse(body)
			} else {
				err = observer.ObserveChunk(body)
			}
			var providerErr *model.ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind != model.ProviderErrorRateLimit || !providerErr.Retryable || providerErr.RetryAfter != 2*time.Second ||
				providerErr.StatusCode != 0 || providerErr.Code != "429" {
				t.Fatalf("provider error=%v", err)
			}
			if err := observer.End(nil); !errors.As(err, &providerErr) {
				t.Fatalf("End lost provider error: %v", err)
			}
			usage := observer.Usage()
			if usage.Status != test.status || usage.Tokens.Known().TotalTokens != 7 || usage.RequestID != "req-fallback" || usage.ResponseID != "failed-response" || usage.Model != "actual-model" {
				t.Fatalf("usage=%+v", usage)
			}
			if observer.Complete() != test.complete {
				t.Fatalf("completion=%t, want %t", observer.Complete(), test.complete)
			}
		})
	}
	observer := newObserver(t, 1)
	if err := observer.ObserveChunk([]byte(`{"candidates":[{"index":0}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`)); err != nil {
		t.Fatal(err)
	}
	raw := observer.RawUsage()
	if err := observer.ObserveChunk([]byte(`{"usageMetadata":{"candidatesTokenCount":"invalid"}}`)); !errors.Is(err, model.ErrProviderFailure) {
		t.Fatalf("malformed usage error=%v", err)
	}
	if observer.Usage().Tokens.Known().TotalTokens != 6 || observer.Usage().Status != model.UsagePartial || !bytes.Equal(raw, observer.RawUsage()) {
		t.Fatalf("last valid usage=%+v raw=%s", observer.Usage(), observer.RawUsage())
	}
}

func newObserver(t *testing.T, candidateCount int) *google.Observer {
	t.Helper()
	connection, err := transport.New(transport.Config{API: "gemini", Provider: "google", APIModel: "test-model", Endpoint: "https://example.test/models/test-model:streamGenerateContent", Authorizer: transport.BearerToken("unused")})
	if err != nil {
		t.Fatal(err)
	}
	return google.NewObserver(connection, candidateCount)
}
