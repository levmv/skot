package provider_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func googleResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"req-google"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestGoogleErrorsPreserveRetryGuidance(t *testing.T) {
	for _, test := range []struct {
		name, details, header string
		want                  time.Duration
	}{
		{name: "quota retry", header: "1", details: `[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"subject":"requests","description":"rate limit"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"52.034842833s"}]`, want: 52*time.Second + 34842833*time.Nanosecond},
		{name: "header longer", header: "60", details: `[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"10s"}]`, want: time.Minute},
		{name: "malformed details", header: "2", details: `[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"invalid"}]`, want: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"error":{"code":429,"message":"opaque provider error","details":%s}}`, test.details)
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {test.header}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			selected, err := provider.Open(t.Context(), provider.Config{URI: "google/gemini-4-argon", APIKey: "fixture", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			_, err = selected.Backend.Complete(t.Context(), model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "hello"}}}, nil)
			var providerError *model.ProviderError
			if !errors.As(err, &providerError) || providerError.Kind != model.ProviderErrorRateLimit || !providerError.Retryable || providerError.RetryAfter != test.want {
				t.Fatalf("error = %v, metadata = %+v, want delay = %s", err, providerError, test.want)
			}
		})
	}
}

func TestGoogleRoutingAndSignedConversationReplay(t *testing.T) {
	const toolSchema = `{"type":"object","properties":{"q":{"type":"string"}}}`
	t.Setenv("OPENCODE_API_KEY", "go-fixture")
	t.Setenv("GEMINI_API_KEY", "google-fixture")
	for _, uri := range []string{"google/gemini-4-argon", "opencode-go/gemini-4-argon-contributor"} {
		t.Run(uri, func(t *testing.T) {
			calls := 0
			var savedParts []jsontext.Value
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				native := strings.HasPrefix(uri, "google/")
				wantHost, wantPrefix, wantKey := "opencode.ai", "/inference/go/v1/models/", "go-fixture"
				if native {
					wantHost, wantPrefix, wantKey = "generativelanguage.googleapis.com", "/v1beta/models/", "google-fixture"
				}
				_, name, _ := strings.Cut(uri, "/")
				if request.URL.Host != wantHost || request.URL.Path != wantPrefix+name+":streamGenerateContent" || request.URL.RawQuery != "alt=sse" {
					t.Errorf("Google endpoint = %s", request.URL)
				}
				if request.Header.Get("x-goog-api-key") != wantKey || request.Header.Get("Authorization") != "" {
					t.Error("incorrect Google authorization")
				}
				if request.Header.Get("X-Session-ID") != "conversation-1" || (!native && request.Header.Get("x-opencode-session") != "conversation-1") {
					t.Error("missing session headers")
				}
				var body struct {
					Contents []struct {
						Role  string           `json:"role"`
						Parts []jsontext.Value `json:"parts"`
					} `json:"contents"`
					SystemInstruction struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"systemInstruction"`
					GenerationConfig struct {
						CandidateCount  int `json:"candidateCount"`
						MaxOutputTokens int `json:"maxOutputTokens"`
						ThinkingConfig  struct {
							Level           string `json:"thinkingLevel"`
							IncludeThoughts bool   `json:"includeThoughts"`
						} `json:"thinkingConfig"`
					} `json:"generationConfig"`
					Tools []struct {
						Functions []struct {
							Name   string         `json:"name"`
							Schema jsontext.Value `json:"parametersJsonSchema"`
						} `json:"functionDeclarations"`
					} `json:"tools"`
				}
				if err := json.UnmarshalRead(request.Body, &body); err != nil {
					t.Fatal(err)
				}
				if body.GenerationConfig.CandidateCount != 1 || body.GenerationConfig.MaxOutputTokens != 1024 || body.GenerationConfig.ThinkingConfig.Level != "high" || !body.GenerationConfig.ThinkingConfig.IncludeThoughts {
					t.Errorf("generation config = %+v", body.GenerationConfig)
				}
				if len(body.Tools) != 1 || len(body.Tools[0].Functions) != 1 || body.Tools[0].Functions[0].Name != "lookup" {
					t.Fatalf("function declarations = %+v", body.Tools)
				}
				assertContentJSON(t, body.Tools[0].Functions[0].Schema, toolSchema)
				if len(body.SystemInstruction.Parts) != 1 || body.SystemInstruction.Parts[0].Text != "Be precise\n\nConversation summary:\nEarlier context" {
					t.Error("lost instructions or summary")
				}
				switch calls {
				case 1:
					if len(body.Contents) != 1 || body.Contents[0].Role != "user" {
						t.Errorf("initial contents = %+v", body.Contents)
					}
					partJSON := []string{`{"text":"Look ","thought":true}`, `{"text":"it up","thought":true}`, `{"text":"Checking","thoughtSignature":"text-signature","futureMetadata":{"version":1}}`, `{"functionCall":{"id":"native-1","name":"lookup","args":{"q":"first"}},"thoughtSignature":"call-signature"}`, `{"functionCall":{"name":"lookup","args":{"q":"second"}}}`, `{"functionCall":{"name":"lookup"}}`, `{"text":"","thoughtSignature":"standalone-signature"}`}
					var stream strings.Builder
					for _, part := range partJSON {
						savedParts = append(savedParts, jsontext.Value(part))
						fmt.Fprintf(&stream, "data: {\"responseId\":\"resp-google\",\"modelVersion\":\"actual-gemini\",\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[%s]}}]}\n\n", part)
					}
					stream.WriteString("data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n")
					stream.WriteString("data: {\"usageMetadata\":{\"promptTokenCount\":20,\"candidatesTokenCount\":5,\"thoughtsTokenCount\":7,\"cachedContentTokenCount\":12}}\n\n")
					return googleResponse(stream.String()), nil
				case 2:
					if len(body.Contents) != 3 || body.Contents[1].Role != "model" || body.Contents[2].Role != "user" {
						t.Fatalf("replayed contents = %+v", body.Contents)
					}
					var want, got any
					original, _ := json.Marshal(savedParts)
					replayed, _ := json.Marshal(body.Contents[1].Parts)
					_ = json.Unmarshal(original, &want)
					_ = json.Unmarshal(replayed, &got)
					if !reflect.DeepEqual(want, got) {
						t.Fatalf("signed parts changed: %s", replayed)
					}
					if len(body.Contents[2].Parts) != 3 {
						t.Fatalf("function responses = %s", body.Contents[2].Parts)
					}
					for i, part := range body.Contents[2].Parts {
						var value struct {
							Response struct {
								ID       string            `json:"id"`
								Name     string            `json:"name"`
								Response map[string]string `json:"response"`
							} `json:"functionResponse"`
						}
						if err := json.Unmarshal(part, &value); err != nil {
							t.Fatal(err)
						}
						if value.Response.Name != "lookup" || value.Response.Response["output"] != "found" || (i == 0 && value.Response.ID != "native-1") || (i > 0 && value.Response.ID != "") {
							t.Errorf("function response = %s", part)
						}
					}
					return googleResponse("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Done\"}]},\"finishReason\":\"STOP\"}]}\n\n"), nil
				case 3:
					modelParts, _ := json.Marshal(body.Contents[1].Parts)
					if !strings.Contains(string(modelParts), `"text":"edited"`) || !strings.Contains(string(modelParts), "skip_thought_signature_validator") || strings.Contains(string(modelParts), "text-signature") || strings.Contains(string(modelParts), "call-signature") {
						t.Fatalf("stale signed history replayed: %s", modelParts)
					}
					return googleResponse("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Done\"}]},\"finishReason\":\"STOP\"}]}\n\n"), nil
				default:
					t.Fatal("unexpected generation or retry")
				}
				return nil, io.ErrUnexpectedEOF
			})}
			selected, err := provider.Open(t.Context(), provider.Config{URI: uri, ReasoningEffort: "high", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if selected.Info.ContextWindow != 1_048_576 || selected.Info.ContextWindowEstimated || selected.Connection.API() != "gemini" || !strings.HasPrefix(selected.Info.BackendID, "gemini.") || selected.Info.ProviderStateContract == "" {
				t.Fatalf("route = %+v", selected.Info)
			}
			replay, err := (model.ReplayContext{}).ForModel(selected.Info)
			if err != nil {
				t.Fatal(err)
			}
			request := model.Request{SessionID: "conversation-1", Instructions: "Be precise", Summary: "Earlier context", MaxOutputTokens: 1024, Items: []model.Item{{Kind: model.ItemUserText, Text: "Look it up"}}, Tools: []model.ToolSpec{{Name: "lookup", InputSchema: jsontext.Value(toolSchema)}}}
			var text, thoughts string
			response, err := selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), func(event model.StreamEvent) {
				if event.Kind == model.EventTextDelta {
					text += event.Text
				} else {
					thoughts += event.Text
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if text != "Checking" || thoughts != "Look it up" || response.StopReason != "tool_calls" {
				t.Fatalf("response = %+v, text=%q, thoughts=%q", response, text, thoughts)
			}
			if response.Usage.Status != model.UsageFinal || response.Usage.ResponseID != "resp-google" || response.Usage.RequestID != "req-google" || response.Usage.Model != "actual-gemini" || response.Usage.Tokens.Known() != (model.TokenCounts{InputTokens: 20, OutputTokens: 12, ReasoningTokens: 7, CachedInputTokens: 12, TotalTokens: 32}) {
				t.Fatalf("usage = %+v, counts = %+v", response.Usage, response.Usage.Tokens.Known())
			}
			accepted, err := replay.AcceptResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			serialized, _ := json.Marshal(accepted.Items)
			var restored []model.Item
			if err := json.Unmarshal(serialized, &restored); err != nil {
				t.Fatal(err)
			}
			request.Items = append(request.Items, restored...)
			for _, item := range restored {
				if item.ToolCall != nil {
					request.Items = append(request.Items, model.Item{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: item.ToolCall.ID, Content: model.TextContent("found")}})
				}
			}
			response, err = selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), nil)
			if err != nil || len(response.Items) != 1 || response.Items[0].Text != "Done" {
				t.Fatalf("continued response=%+v error=%v", response, err)
			}
			if response.Usage.Status != model.UsageUnavailable || response.Usage.Tokens.InputTokens != nil {
				t.Fatalf("invented missing usage: %+v", response.Usage)
			}
			for i := range request.Items {
				if request.Items[i].Kind == model.ItemAssistantText {
					request.Items[i].Text = "edited"
					break
				}
			}
			_, err = selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), nil)
			if err != nil || calls != 3 {
				t.Fatalf("edited signed history: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestGoogleReplaysToolsAfterModelChange(t *testing.T) {
	for _, change := range []string{"model", "protocol"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return googleResponse(`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"old-native-id","name":"lookup","args":{}},"thoughtSignature":"old-signature"}]},"finishReason":"STOP"}]}` + "\n\n"), nil
				}
				var body struct {
					Contents []struct {
						Parts []struct {
							Signature string `json:"thoughtSignature"`
							Call      *struct {
								ID   string         `json:"id"`
								Name string         `json:"name"`
								Args map[string]any `json:"args"`
							} `json:"functionCall"`
							Result *struct {
								ID       string            `json:"id"`
								Name     string            `json:"name"`
								Response map[string]string `json:"response"`
							} `json:"functionResponse"`
						} `json:"parts"`
					} `json:"contents"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Contents) != 3 || len(body.Contents[1].Parts) != 1 || len(body.Contents[2].Parts) != 1 {
					t.Fatalf("lost tool history: %+v", body)
				}
				call, result := body.Contents[1].Parts[0], body.Contents[2].Parts[0]
				if call.Signature != "skip_thought_signature_validator" || call.Call == nil || call.Call.ID != "" || call.Call.Name != "lookup" || call.Call.Args == nil {
					t.Fatalf("invalid transferred call: %+v", call)
				}
				if result.Result == nil || result.Result.ID != "" || result.Result.Name != "lookup" || result.Result.Response["output"] != "found" {
					t.Fatalf("invalid transferred result: %+v", result)
				}
				return googleResponse(`data: {"candidates":[{"content":{"parts":[{"text":"Done"}]},"finishReason":"STOP"}]}` + "\n\n"), nil
			})}
			config := provider.Config{URI: "google/gemini-4-argon", APIKey: "fixture", HTTPClient: client}
			selected, err := provider.Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := (model.ReplayContext{}).ForModel(selected.Info)
			if err != nil {
				t.Fatal(err)
			}
			request := model.Request{Items: []model.Item{{Kind: model.ItemUserText, Text: "look it up"}}}
			response, err := selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), nil)
			if err != nil {
				t.Fatal(err)
			}
			if change == "protocol" {
				replay, err = replay.ForModel(model.Info{BackendID: "chat_completions.example", Provider: "example", Model: "example"})
				if err != nil {
					t.Fatal(err)
				}
				response.Items = []model.Item{{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "lookup", RawArguments: "{}"}}}
			}
			response, err = replay.AcceptResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			request.Items = append(request.Items, response.Items...)
			for _, item := range response.Items {
				if item.ToolCall != nil {
					request.Items = append(request.Items, model.Item{Kind: model.ItemToolResult, ToolResult: &model.ToolResult{CallID: item.ToolCall.ID, Content: model.TextContent("found")}})
				}
			}
			if change == "model" {
				config.URI = "google/gemini-3.8-flash"
			}
			selected, err = provider.Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			replay, err = replay.ForModel(selected.Info)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := selected.Backend.Complete(t.Context(), replay.PrepareRequest(selected.Backend, request), nil); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
