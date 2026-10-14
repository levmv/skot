package google_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/google"
	"github.com/levmv/skot/model/transport"
)

func TestCompletionRules(t *testing.T) {
	for _, test := range []struct {
		name, body, stop, text string
		fail                   bool
	}{
		{name: "truncated", body: `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`, fail: true},
		{name: "unknown finish", body: `{"candidates":[{"finishReason":"FUTURE_STOP"}]}`, fail: true},
		{name: "blocked", body: `{"promptFeedback":{"blockReason":"SAFETY"}}`, stop: "content_filter"},
		{name: "length", body: `{"candidates":[{"content":{"parts":[{"text":"partial"},{"functionCall":{"name":"lookup","args":{"x":1}}}]},"finishReason":"MAX_TOKENS"}]}`, stop: "max_tokens", text: "partial"},
		{name: "continuation", body: `{"candidates":[{"content":{"parts":[{"text":"partial"},{"functionCall":{"name":"lookup"}}]},"finishReason":"CONTINUATION","continuationToken":"opaque"}]}`, stop: "incomplete", text: "partial"},
		{name: "unsupported output", body: `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AQ=="}}]},"finishReason":"STOP"}]}`, fail: true},
		{name: "unrecognized content", body: `{"candidates":[{"content":{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"https://example.test/image"}}]},"finishReason":"STOP"}]}`, fail: true},
		{name: "multiple candidates", body: `{"candidates":[{"index":0,"finishReason":"STOP"},{"index":1,"finishReason":"STOP"}]}`, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newBackend(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return googleResponse("data: " + test.body + "\n\n"), nil
			})})
			response, err := backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil)
			if (err != nil) != test.fail || (!test.fail && response.StopReason != test.stop) {
				t.Fatalf("response=%+v error=%v", response, err)
			}
			var text string
			for _, item := range response.Items {
				if item.Kind != model.ItemAssistantText {
					t.Fatalf("unexpected partial item: %+v", item)
				}
				text += item.Text
			}
			if text != test.text {
				t.Fatalf("text=%q, want %q", text, test.text)
			}
		})
	}
}

func TestGenerationFailures(t *testing.T) {
	for _, test := range []struct {
		reason string
		retry  bool
		kind   model.ProviderErrorKind
	}{
		{reason: "MALFORMED_FUNCTION_CALL", retry: true},
		{reason: "UNEXPECTED_TOOL_CALL", retry: true},
		{reason: "MALFORMED_RESPONSE", retry: true},
		{reason: "MISSING_THOUGHT_SIGNATURE", kind: model.ProviderErrorRequest},
	} {
		t.Run(test.reason, func(t *testing.T) {
			backend := newBackend(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return googleResponse(fmt.Sprintf("data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"lookup\",\"args\":{}}}]},\"finishReason\":%q}]}\n\ndata: {\"usageMetadata\":{\"promptTokenCount\":10,\"thoughtsTokenCount\":5}}\n\n", test.reason)), nil
			})})
			response, err := backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil)
			var providerErr *model.ProviderError
			if !errors.As(err, &providerErr) || !errors.Is(err, model.ErrProviderFailure) || providerErr.Code != test.reason || providerErr.Retryable != test.retry || providerErr.Kind != test.kind {
				t.Fatalf("error=%v metadata=%+v", err, providerErr)
			}
			if len(response.Items) != 0 || response.Usage.Status != model.UsageFinal || response.Usage.Tokens.Known().TotalTokens != 15 {
				t.Fatalf("failed generation returned calls or lost late usage: %+v", response)
			}
		})
	}
}

func TestDefaultThinkingLevel(t *testing.T) {
	backend := newBackend(t, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Config struct {
				Thinking struct {
					Level   *string `json:"thinkingLevel"`
					Include bool    `json:"includeThoughts"`
				} `json:"thinkingConfig"`
			} `json:"generationConfig"`
		}
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Fatal(err)
		}
		if !body.Config.Thinking.Include || body.Config.Thinking.Level != nil {
			t.Fatalf("default thinking config=%+v", body.Config.Thinking)
		}
		return googleResponse("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Done\"}]},\"finishReason\":\"STOP\"}]}\n\n"), nil
	})})
	if _, err := backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestImagesAndFunctionResults(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Contents []struct {
				Role  string                      `json:"role"`
				Parts []map[string]jsontext.Value `json:"parts"`
			} `json:"contents"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Contents) != 3 || len(body.Contents[0].Parts) != 2 {
			t.Fatalf("contents=%+v", body.Contents)
		}
		var tool struct {
			Name     string                      `json:"name"`
			Response map[string]string           `json:"response"`
			Parts    []map[string]jsontext.Value `json:"parts"`
		}
		if err := json.Unmarshal(body.Contents[2].Parts[0]["functionResponse"], &tool); err != nil || tool.Name != "screenshot" || tool.Response["output"] != "captured" || len(tool.Parts) != 1 {
			t.Fatalf("function response=%+v error=%v", tool, err)
		}
		for source, raw := range map[string]jsontext.Value{
			"user":        body.Contents[0].Parts[1]["inlineData"],
			"tool result": tool.Parts[0]["inlineData"],
		} {
			var image struct {
				MIMEType string `json:"mimeType"`
				Data     []byte `json:"data"`
			}
			if err := json.Unmarshal(raw, &image); err != nil || image.MIMEType != "image/png" || string(image.Data) != "image-fixture" {
				t.Fatalf("%s image=%+v error=%v", source, image, err)
			}
		}
		return googleResponse("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Done\"}]},\"finishReason\":\"STOP\"}]}\n\n"), nil
	})}
	backend := newBackend(t, client)
	image := model.ImageContent{MediaType: "image/png", Data: []byte("image-fixture"), Width: 1, Height: 1}
	request := model.Request{Items: []model.Item{
		{Kind: model.ItemUserText, Content: model.ImageToolContent("look", image)},
		{Kind: model.ItemToolCall, ResponseID: "response", ToolCall: &model.ToolCall{ID: "local-call", Name: "screenshot", RawArguments: "{}"}},
		{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "local-call", Content: model.ImageToolContent("captured", image)}},
	}}
	if _, err := backend.Complete(t.Context(), request, nil); err != nil {
		t.Fatal(err)
	}
}

func newBackend(t *testing.T, client *http.Client) *google.Backend {
	t.Helper()
	connection, err := transport.New(transport.Config{API: "gemini", Provider: "google", APIModel: "test-model", Endpoint: "https://example.test/models/test-model:streamGenerateContent", HTTPClient: client, Authorizer: transport.BearerToken("unused")})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := google.New(google.Config{Connection: connection})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func googleResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}
