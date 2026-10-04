package agent

import (
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"

	modelapi "github.com/levmv/skot/model"
)

func TestProviderDataIsClonedButNotSanitized(t *testing.T) {
	runtime := &Runtime{sanitize: func(value string) string { return strings.ReplaceAll(value, "secret", "[redacted]") }}
	original := []modelapi.Item{{
		Kind: modelapi.ItemReasoning, Text: "visible secret",
		ProviderData: []modelapi.ProviderData{{Kind: "responses.reasoning_item", Data: jsontext.Value(`{"encrypted_content":"secret"}`)}},
	}}
	sanitized := runtime.sanitizeItems(original)
	if sanitized[0].Text != "visible [redacted]" || string(sanitized[0].ProviderData[0].Data) != `{"encrypted_content":"secret"}` {
		t.Fatalf("sanitized item = %#v", sanitized[0])
	}
	sanitized[0].ProviderData[0].Data[2] = 'X'
	if string(original[0].ProviderData[0].Data) != `{"encrypted_content":"secret"}` {
		t.Fatal("sanitized provider data aliases its source")
	}
}

func TestReplayRejectsInvalidReasoningProviderData(t *testing.T) {
	records := []Record{
		recordForTest(t, 1, RecordSessionStarted, SessionStartedRecord{SchemaVersion: JournalSchemaVersion, SessionID: "session"}),
		recordForTest(t, 2, RecordModelSelected, modelapi.ReplayContext{Backend: "responses.openai", Provider: "openai", Model: "model", Epoch: "epoch"}),
		recordForTest(t, 3, RecordRunStarted, RunStartedRecord{RunID: "run"}),
		recordForTest(t, 4, RecordRunInputAdded, RunInputAddedRecord{RunID: "run", Text: "hello"}),
		recordForTest(t, 5, RecordModelResponse, ModelResponseRecord{
			RunID: "run", Backend: "responses.openai", Model: "model", Epoch: "epoch",
			Items: []modelapi.Item{{
				Kind: modelapi.ItemReasoning, ResponseID: "response", ProviderContext: &modelapi.ProviderContext{Backend: "responses.openai", Epoch: "epoch"},
				ProviderData: []modelapi.ProviderData{{Kind: " responses.reasoning_item ", Data: jsontext.Value(`{}`)}},
			}},
		}),
	}
	if _, err := Replay(records); err == nil || !strings.Contains(err.Error(), "provider data") {
		t.Fatalf("Replay() error = %v", err)
	}
}

func TestOpaqueProviderDataDoesNotAffectTokenEstimate(t *testing.T) {
	without := estimateItemsTokens([]modelapi.Item{{Kind: modelapi.ItemReasoning, Text: "summary"}})
	with := estimateItemsTokens([]modelapi.Item{{
		Kind: modelapi.ItemReasoning, Text: "summary",
		ProviderData: []modelapi.ProviderData{{Kind: "responses.reasoning_item", Data: jsontext.Value(`{"encrypted_content":"` + strings.Repeat("x", 4096) + `"}`)}},
	}})
	if with != without {
		t.Fatalf("opaque bytes changed token estimate: %d != %d", with, without)
	}
}

func TestRuntimeJournalsAndReplaysProviderDataAcrossToolTurn(t *testing.T) {
	journal := &memoryJournal{}
	state := jsontext.Value(`{"id":"rs_1","encrypted_content":"ciphertext"}`)
	model := &scriptedModel{
		info: modelapi.Info{
			BackendID: "responses.test", Provider: "test", Model: "model",
			ProviderStateContract: "responses.manual_history.v1",
		},
		steps: []modelStep{
			func(context.Context, modelapi.Request, func(modelapi.StreamEvent)) (modelapi.Response, error) {
				return modelapi.Response{Items: []modelapi.Item{
					{Kind: modelapi.ItemReasoning, Text: "safe summary", ProviderData: []modelapi.ProviderData{{Kind: "responses.reasoning_item", Data: state}}},
					{Kind: modelapi.ItemToolCall, ToolCall: &modelapi.ToolCall{Name: "inspect", RawArguments: `{}`}},
				}}, nil
			},
			func(_ context.Context, request modelapi.Request, _ func(modelapi.StreamEvent)) (modelapi.Response, error) {
				var reasoning *modelapi.Item
				for index := range request.Items {
					if request.Items[index].Kind == modelapi.ItemReasoning {
						reasoning = &request.Items[index]
					}
				}
				if reasoning == nil || reasoning.ProviderContext == nil ||
					reasoning.ProviderContext.Backend != "responses.test" || reasoning.ProviderContext.Epoch != request.ProviderEpoch ||
					len(reasoning.ProviderData) != 1 || string(reasoning.ProviderData[0].Data) != string(state) {
					t.Fatalf("replayed reasoning = %#v, request epoch = %q", reasoning, request.ProviderEpoch)
				}
				return modelapi.Response{Items: []modelapi.Item{{Kind: modelapi.ItemAssistantText, Text: "done"}}}, nil
			},
		},
	}
	runtime := newTestRuntime(t, Config{
		Backend: model, Journal: journal,
		Tools: []Tool{{
			Spec: modelapi.ToolSpec{Name: "inspect", InputSchema: jsontext.Value(`{"type":"object"}`)},
			Run: func(context.Context, string) (ToolOutput, error) {
				return ToolOutput{Content: modelapi.TextContent("ok")}, nil
			},
		}},
	})
	if _, err := runtime.Run(context.Background(), "inspect", nil); err != nil {
		t.Fatal(err)
	}
	replayed, err := Replay(journal.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var persisted *modelapi.Item
	for index := range replayed.Items {
		if replayed.Items[index].Kind == modelapi.ItemReasoning {
			persisted = &replayed.Items[index]
		}
	}
	if persisted == nil || len(persisted.ProviderData) != 1 || string(persisted.ProviderData[0].Data) != string(state) {
		t.Fatalf("persisted reasoning = %#v", persisted)
	}
}
