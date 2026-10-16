package provider_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func TestToolChoiceRetainsSchemasAndCompletedToolHistory(t *testing.T) {
	for _, api := range []provider.API{provider.ChatCompletions, provider.Responses, provider.AnthropicMessages, provider.Gemini} {
		t.Run(string(api), func(t *testing.T) {
			var body []byte
			config := explicitTestConfig(api)
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var err error
				body, err = io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"request captured"}}`))}, nil
			})}
			selected, err := provider.New(config)
			if err != nil {
				t.Fatal(err)
			}
			request := model.Request{
				Tools: []model.ToolSpec{{Name: "lookup", InputSchema: jsontext.Value(`{"type":"object","properties":{"query":{"type":"string"}}}`)}},
				Items: []model.Item{
					{Kind: model.ItemUserText, Text: "look it up"},
					{Kind: model.ItemToolCall, ResponseID: "previous", ToolCall: &model.ToolCall{ID: "call-1", Name: "lookup", RawArguments: `{"query":"answer"}`}},
					{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: "call-1", Content: model.TextContent("lookup result")}},
				},
			}
			var defaultRequest map[string]jsontext.Value
			for _, choice := range []model.ToolChoice{"", model.ToolChoiceAuto, model.ToolChoiceNone, "unsupported"} {
				request.ToolChoice, body = choice, nil
				_, err := selected.Backend.Complete(t.Context(), request, nil)
				if choice == "unsupported" {
					if !errors.Is(err, model.ErrInvalidRequest) || !errors.Is(err, model.ErrRequestNotSent) || body != nil {
						t.Fatalf("unsupported choice sent: body=%s, error=%v", body, err)
					}
					continue
				}
				if body == nil || errors.Is(err, model.ErrRequestNotSent) {
					t.Fatalf("choice %q: body=%s, error=%v", choice, body, err)
				}
				var fields map[string]jsontext.Value
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				field, want := "tool_choice", ""
				if api == provider.Gemini {
					field = "toolConfig"
				}
				if choice != "" {
					switch api {
					case provider.AnthropicMessages:
						want = `{"type":"` + string(choice) + `"}`
					case provider.Gemini:
						want = `{"functionCallingConfig":{"mode":"` + strings.ToUpper(string(choice)) + `"}}`
					default:
						want = `"` + string(choice) + `"`
					}
				}
				if string(fields[field]) != want {
					t.Fatalf("%s = %s, want %s", field, fields[field], want)
				}
				delete(fields, field)
				if choice == "" {
					if !strings.Contains(string(fields["tools"]), `"lookup"`) || !strings.Contains(string(fields["tools"]), `"query"`) ||
						!strings.Contains(string(body), `"lookup result"`) || !strings.Contains(string(body), "answer") {
						t.Fatalf("default request lacks schemas or tool history: %s", body)
					}
					defaultRequest = fields
				} else if !reflect.DeepEqual(fields, defaultRequest) {
					t.Fatalf("choice %q changed request fields other than %s: %s", choice, field, body)
				}
			}
		})
	}
}
