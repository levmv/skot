package agent

import (
	"context"
	"encoding/json/jsontext"
	"reflect"
	"testing"
)

func TestLiveRequestsEqualFullReplayProjection(t *testing.T) {
	journal := &memoryJournal{}
	var runtime *Runtime
	requestNumber := 0
	model := configurationModel{
		info: ModelInfo{BackendID: "test", Provider: "test", Model: "projection", ContextWindow: 128_000},
		complete: func(_ context.Context, request ModelRequest, _ func(ModelStreamEvent)) (ModelResponse, error) {
			replayed, err := Replay(journal.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			expected, err := runtime.modelRequestForRun(replayed, runRequestSpec{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(request, expected) {
				t.Fatalf("live request differs from full replay\nlive: %#v\nreplay: %#v", request, expected)
			}
			requestNumber++
			if requestNumber == 1 {
				return ModelResponse{Items: []Item{{Kind: ItemToolCall, ToolCall: &ToolCall{
					ID: "provider-call", Name: "echo", RawArguments: `{"text":"hello"}`,
				}}}}, nil
			}
			return ModelResponse{Items: []Item{{Kind: ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
		},
	}
	tool := Tool{
		Spec: ToolSpec{Name: "echo", Description: "echo input", InputSchema: jsontext.Value(`{"type":"object"}`)},
		Run: func(_ context.Context, arguments string) (ToolOutput, error) {
			return ToolOutput{Content: TextContent(arguments)}, nil
		},
	}
	boundaryCommitted := false
	runtime = newTestRuntime(t, Config{
		Backend: model, Journal: journal, Tools: []Tool{tool}, Instructions: "be exact",
		Metadata: ConfigurationMetadata{ToolSet: "default"},
		ExternalWork: externalWorkFuncs{
			pending: func(string) []BoundaryEvent {
				if boundaryCommitted {
					return nil
				}
				return []BoundaryEvent{{JobID: "job-projection", Content: "background result"}}
			},
			committed: func(string) { boundaryCommitted = true },
		},
	})
	if err := runtime.QueueInput("queued input"); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "initial input", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" || requestNumber != 2 || !boundaryCommitted {
		t.Fatalf("result/requests/committed = %#v/%d/%v", result, requestNumber, boundaryCommitted)
	}
}
