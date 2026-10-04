package agent

import (
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"

	modelapi "github.com/levmv/skot/model"
)

func TestRuntimeNormalizesToolArgumentsBeforeExecutionAndJournaling(t *testing.T) {
	t.Run("empty provider arguments", func(t *testing.T) {
		journal := &memoryJournal{}
		executedWith := ""
		model := &scriptedModel{steps: []modelStep{
			func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
				return modelapi.Response{Items: []modelapi.Item{{
					Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "inspect", RawArguments: " \n "},
				}}}, nil
			},
			func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
				return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "done"}}}, nil
			},
		}}
		runtime := newTestRuntime(t, Config{
			Backend: model, Journal: journal,
			Tools: []Tool{{
				Spec: modelapi.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
				Run: func(_ context.Context, raw string) (ToolOutput, error) {
					executedWith = raw
					return ToolOutput{Content: modelapi.TextContent("ok")}, nil
				},
			}},
		})
		result, err := runtime.Run(context.Background(), "inspect", nil)
		if err != nil || result.Status != RunCompleted || executedWith != `{}` {
			t.Fatalf("result/error/arguments = %#v / %v / %q", result, err, executedWith)
		}
		journaled := false
		for _, record := range journal.snapshot() {
			if record.Kind != RecordModelResponse {
				continue
			}
			response, err := record.decode[ModelResponseRecord]()
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range response.Items {
				if item.ToolCall == nil {
					continue
				}
				journaled = true
				if item.ToolCall.RawArguments != `{}` {
					t.Fatalf("journaled arguments = %q", item.ToolCall.RawArguments)
				}
			}
		}
		if !journaled {
			t.Fatal("tool call was not journaled")
		}
	})

	t.Run("non-object provider arguments", func(t *testing.T) {
		journal := &memoryJournal{}
		model := &scriptedModel{steps: []modelStep{func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
			return modelapi.Response{Items: []modelapi.Item{{
				Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "inspect", RawArguments: `[]`},
			}}}, nil
		}}}
		runtime := newTestRuntime(t, Config{
			Backend: model, Journal: journal,
			Tools: []Tool{{
				Spec: modelapi.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
				Run:  func(context.Context, string) (ToolOutput, error) { return ToolOutput{}, nil },
			}},
		})
		result, err := runtime.Run(context.Background(), "inspect", nil)
		if result.Status != RunFailed || err == nil || !strings.Contains(err.Error(), "invalid tool arguments") {
			t.Fatalf("result/error = %#v / %v", result, err)
		}
		if got := countRecordKind(journal.snapshot(), RecordModelResponse); got != 0 {
			t.Fatalf("invalid model responses journaled = %d", got)
		}
	})
}

func TestReplayNormalizesStoredToolArguments(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, modelapi.ReplayContext{Backend: "test", Provider: "test", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "render"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "test", Model: "model", Epoch: "epoch",
			Items: []modelapi.Item{{Kind: modelapi.ItemToolCall, ResponseID: "response", ToolCall: &modelapi.ToolCall{
				ID: "call", Name: "render", RawArguments: ` { "markup": "<p>&</p>" } `,
			}}},
		}),
	}
	state, err := Replay(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingTools) != 1 || state.PendingTools[0].Call.RawArguments != `{"markup":"<p>&</p>"}` {
		t.Fatalf("pending tools = %#v", state.PendingTools)
	}
}
