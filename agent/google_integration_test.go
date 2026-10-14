package agent_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/provider"
)

func TestGoogleRedactionPreservesToolHistoryAcrossResume(t *testing.T) {
	const secret = "fixture-secret-value"
	const signature = "c2lnbmVkLXRvb2wtc3RhdGU="
	const emptySignature = "c2lnbmVkLWVtcHR5LXN0YXRl"
	for _, changedField := range []string{"none", "text", "thought", "arguments"} {
		t.Run(changedField, func(t *testing.T) {
			text, thought, argument := "checking", "looking", "public"
			switch changedField {
			case "text":
				text = "checking " + secret
			case "thought":
				thought = "looking " + secret
			case "arguments":
				argument = secret
			}
			redacted := changedField != "none"
			// Split text across SSE parts so redaction must compare the complete
			// semantic text, not independently sanitize each original part.
			parts := []map[string]any{
				{"thought": true, "text": thought[:len(thought)/2]},
				{"thought": true, "text": thought[len(thought)/2:]},
				{"text": text[:len(text)/2]},
				{"text": text[len(text)/2:]},
				{"functionCall": map[string]any{"id": "native-call", "name": "inspect", "args": map[string]string{"value": argument}}, "thoughtSignature": signature},
				{"text": "", "thoughtSignature": emptySignature},
			}
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), secret) {
					t.Fatal("secret reached a model request")
				}
				var stream strings.Builder
				if requests == 1 {
					for _, part := range parts {
						data, err := json.Marshal(part)
						if err != nil {
							t.Fatal(err)
						}
						fmt.Fprintf(&stream, "data: {\"candidates\":[{\"content\":{\"parts\":[%s]}}]}\n\n", data)
					}
					stream.WriteString("data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n")
				} else {
					for _, original := range []string{signature, emptySignature} {
						if strings.Contains(string(body), original) == redacted {
							t.Fatalf("request %d: original signature retention disagrees with redaction", requests)
						}
					}
					// Imported history retains assistant text and calls; the old
					// reasoning is omitted when its signed state was discarded.
					visibleRedaction := changedField == "text" || changedField == "arguments"
					if strings.Contains(string(body), "skip_thought_signature_validator") != redacted ||
						strings.Contains(string(body), "[REDACTED]") != visibleRedaction ||
						strings.Count(string(body), `"id":"native-call"`) != 2 {
						t.Fatalf("request %d lost sanitized tool history: %s", requests, body)
					}
					stream.WriteString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Done\"}]},\"finishReason\":\"STOP\"}]}\n\n")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}, nil
			})}
			selected, err := provider.Open(t.Context(), provider.Config{
				URI: "google/gemini-4-argon", APIKey: "fixture", HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			journal := &memoryJournal{}
			toolRuns := 0
			config := agent.Config{
				Model: selected.Info, Backend: selected.Backend, Journal: journal,
				Sanitize: func(text string) string { return strings.ReplaceAll(text, secret, "[REDACTED]") },
				Tools: []agent.Tool{{
					Spec: model.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object","properties":{"value":{"type":"string"}}}`)},
					Run: func(_ context.Context, arguments string) (agent.ToolOutput, error) {
						toolRuns++
						var decoded struct {
							Value string `json:"value"`
						}
						if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
							t.Fatal(err)
						}
						if decoded.Value != strings.ReplaceAll(argument, secret, "[REDACTED]") {
							t.Fatalf("tool arguments were not sanitized: %s", arguments)
						}
						return agent.ToolOutput{Content: model.TextContent("observed")}, nil
					},
				}},
			}
			for _, input := range []string{"inspect", "continue after resume"} {
				// Recreate the runtime to verify that persisted history is enough
				// to continue, without any response state held only in memory.
				runtime, err := agent.New(config)
				if err != nil {
					t.Fatal(err)
				}
				result, err := runtime.Run(t.Context(), input, nil)
				if err != nil || result.Status != agent.RunCompleted || result.Answer != "Done" {
					t.Fatalf("run = %+v, error = %v", result, err)
				}
				raw, err := json.Marshal(journal.snapshot())
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), secret) {
					t.Fatal("plaintext secret reached the journal")
				}
				if strings.Contains(string(raw), "[REDACTED]") != redacted {
					t.Fatal("journal lost sanitized response content")
				}
				if redacted && (strings.Contains(string(raw), signature) || strings.Contains(string(raw), emptySignature)) {
					t.Fatal("invalidated signed state reached the journal")
				}
			}
			if requests != 3 || toolRuns != 1 {
				t.Fatalf("requests = %d, tool runs = %d", requests, toolRuns)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
